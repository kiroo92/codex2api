package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== MiMo 上游执行器 ====================

// MimoUpstreamConfig MiMo 上游配置
type MimoUpstreamConfig struct {
	BaseURL        string // MiMo API 基础 URL
	APIKey         string // MiMo API Key
	IsTokenPlan    bool   // 是否为 Token Plan 账号
	ModelMapping   string // 账号级别模型映射 JSON
}

// GenerateMimoResponseID 生成 MiMo 响应 ID
func GenerateMimoResponseID() string {
	b := make([]byte, 12)
	n, err := rand.Read(b)
	if err != nil || n != len(b) {
		b = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	return "resp_mimo_" + hex.EncodeToString(b)
}

// MimoUsageResult 从 MiMo 上游返回的 usage 数据
type MimoUsageResult struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	CachedTokens   int64
	ReasoningTokens int64
}

// HandleMimoResponsesAPI 处理通过 MiMo 上游的 Responses API 请求
// 流程: Responses API body → Chat Completions body → MiMo upstream → Chat chunks → Responses SSE events
func HandleMimoResponsesAPI(c *gin.Context, responsesBody []byte, cfg *MimoUpstreamConfig) *MimoUsageResult {
	isStream := gjson.GetBytes(responsesBody, "stream").Bool()

	// 翻译请求: Responses → Chat Completions
	chatBody, model, err := ResponsesToMimoChat(responsesBody, cfg.IsTokenPlan)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("failed to translate request: %v", err),
		})
		return nil
	}

	// 应用账号级别模型映射
	model = ApplyAccountModelMapping(model, cfg.ModelMapping)
	chatBody, _ = sjson.SetBytes(chatBody, "model", model)

	// 流式请求时要求上游返回 usage 数据（否则 token 统计为 0）
	if isStream {
		chatBody, _ = sjson.SetBytes(chatBody, "stream_options", map[string]bool{"include_usage": true})
	}

	slog.Debug("mimo upstream request",
		"model", model,
		"stream", isStream,
		"isTokenPlan", cfg.IsTokenPlan,
	)

	// 确定 MiMo API 端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = ResolveMimoBaseURL("", cfg.APIKey)
	}
	apiURL := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"

	// 生成响应 ID
	responseID := GenerateMimoResponseID()

	// 发起上游请求
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", apiURL, bytes.NewReader(chatBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to create request: %v", err),
		})
		return nil
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "OpenAI/1.0 codex2api")

	client := &http.Client{
		Timeout: 0, // 无超时，由 context 控制
	}

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("mimo upstream error: %v", err),
		})
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		slog.Error("mimo upstream error", "status", resp.StatusCode, "body", string(bodyBytes))
		
		// 友好错误处理
		errMsg := string(bodyBytes)
		if resp.StatusCode == 400 && strings.Contains(errMsg, "webSearchEnabled is false") {
			errMsg = "MiMo Web Search 插件未激活。请在 https://platform.xiaomimimo.com/#/console/plugin 激活后，" +
				"设置环境变量 MIMO_FORCE_WEB_SEARCH=true，或移除请求中的 web_search 工具。" +
				"原始错误: " + errMsg
		}
		
		c.JSON(resp.StatusCode, map[string]string{
			"error": errMsg,
		})
		return nil
	}

	if isStream {
		return handleMimoStreamResponse(c, resp.Body, responseID, model)
	}
	return handleMimoNonStreamResponse(c, resp.Body, responseID)
}

// HandleMimoChatCompletionsAPI 处理通过 MiMo 上游的 Chat Completions API 请求
// 流程: Chat Completions body → MiMo 格式化 → MiMo upstream → 原样返回
func HandleMimoChatCompletionsAPI(c *gin.Context, chatBody []byte, cfg *MimoUpstreamConfig) {
	isStream := gjson.GetBytes(chatBody, "stream").Bool()

	// 翻译请求: 标准 Chat → MiMo Chat
	mimoBody, model, err := TranslateChatCompletionsToMimo(chatBody)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("failed to translate request: %v", err),
		})
		return
	}

	// 应用账号级别模型映射
	model = ApplyAccountModelMapping(model, cfg.ModelMapping)
	mimoBody, _ = sjson.SetBytes(mimoBody, "model", model)

	slog.Debug("mimo chat completions request",
		"model", model,
		"stream", isStream,
	)

	// 确定 MiMo API 端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = ResolveMimoBaseURL("", cfg.APIKey)
	}
	apiURL := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"

	// 发起上游请求
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", apiURL, bytes.NewReader(mimoBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to create request: %v", err),
		})
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("User-Agent", "OpenAI/1.0 codex2api")
	if isStream {
		req.Header.Set("Accept", "text/event-stream")
	}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("mimo upstream error: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		
		// 友好错误处理
		errMsg := string(bodyBytes)
		if resp.StatusCode == 400 && strings.Contains(errMsg, "webSearchEnabled is false") {
			errMsg = "MiMo Web Search 插件未激活。请在 https://platform.xiaomimimo.com/#/console/plugin 激活后重试。" +
				"原始错误: " + errMsg
		}
		
		c.JSON(resp.StatusCode, map[string]string{
			"error": errMsg,
		})
		return
	}

	if isStream {
		// 流式: 透传 MiMo 的 SSE 流
		handleMimoPassthroughStream(c, resp.Body)
	} else {
		// 非流式: 透传响应
		c.Header("Content-Type", "application/json")
		io.Copy(c.Writer, resp.Body)
	}
}

// ==================== 流式响应处理 ====================

// handleMimoStreamResponse 处理 MiMo 流式响应，翻译为 Responses SSE 格式
// 完整实现 OpenAI Responses API SSE 生命周期事件序列
func handleMimoStreamResponse(c *gin.Context, body io.Reader, responseID string, model string) *MimoUsageResult {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return handleMimoNonStreamResponse(c, body, responseID)
	}

	// 累积状态
	state := &MimoStreamState{
		ResponseID:      responseID,
		Model:           model,
		FunctionCallSeq: make(map[int]*MimoStreamFunctionCallState),
	}
	reasoningItemOpened := false
	outputItemOpened := false

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var currentData strings.Builder
	var lastUsageData string
	usageResult := &MimoUsageResult{}

	// emit 辅助函数: 写入 SSE 事件并刷新
	emit := func(event string) {
		if event != "" {
			fmt.Fprintf(c.Writer, "%s\n\n", event)
			flusher.Flush()
		}
	}

	// closeReasoningItem 辅助函数: 关闭打开的 reasoning item
	closeReasoningItem := func() {
		if !reasoningItemOpened {
			return
		}
		outputIdx := state.OutputIndex
		seq := state.NextSeq()
		// reasoning_summary_text.done
		emit(buildResponseReasoningTextDoneEvent(state.ReasoningText, responseID, 0, seq))
		// reasoning_summary_part.done
		seq = state.NextSeq()
		emit(buildResponsesReasoningSummaryPartDoneEvent(state.ReasoningText, responseID, outputIdx, seq))
		// output_item.done (带 encrypted_content)
		reasoningItemData := map[string]any{
			"type":              "reasoning",
			"id":                "rs_" + responseID,
			"summary":           []map[string]any{{"type": "summary_text", "text": state.ReasoningText}},
			"encrypted_content": state.ReasoningText,
			"status":            "completed",
		}
		seq = state.NextSeq()
		emit(buildResponseOutputItemDoneEvent(outputIdx, reasoningItemData, seq))
		state.OutputIndex++
		reasoningItemOpened = false
	}

	// closeOutputItem 辅助函数: 关闭打开的 message output item
	closeOutputItem := func() {
		if !outputItemOpened {
			return
		}
		outputIdx := state.OutputIndex
		seq := state.NextSeq()
		// output_text.done
		emit(buildResponseOutputTextDoneEvent(state.OutputText, responseID, seq))
		// content_part.done
		outputPartData := map[string]any{
			"type":        "output_text",
			"text":        state.OutputText,
			"annotations": []any{},
		}
		seq = state.NextSeq()
		emit(buildResponseContentPartDoneEvent(outputPartData, outputIdx, 0, seq))
		// output_item.done
		outputItemData := map[string]any{
			"type": "message",
			"id":   "msg_" + responseID,
			"role": "assistant",
			"content": []map[string]any{
				{"type": "output_text", "text": state.OutputText, "annotations": []any{}},
			},
			"status": "completed",
		}
		seq = state.NextSeq()
		emit(buildResponseOutputItemDoneEvent(outputIdx, outputItemData, seq))
		outputItemOpened = false
	}

	// closeFunctionCallItem 辅助函数: 关闭指定 index 的 function call item
	closeFunctionCallItem := func(idx int) {
		tc, ok := state.FunctionCallSeq[idx]
		if !ok || !tc.Opened {
			return
		}
		// 对 arguments 做 JSON 校验
		safeArgs := sanitizeToolCallArgumentsOutput(tc.ArgsBuffer, tc.Name, tc.CallID, state.FinishReason)
		seq := state.NextSeq()
		// function_call_arguments.done
		emit(buildResponsesFunctionCallArgumentsDoneEvent(tc.ItemID, safeArgs, tc.OutputIdx, seq))
		// output_item.done
		finalItem := map[string]any{
			"id":       tc.ItemID,
			"type":     "function_call",
			"call_id":  tc.CallID,
			"name":     tc.Name,
			"arguments": safeArgs,
			"status":   "completed",
		}
		seq = state.NextSeq()
		emit(buildResponseOutputItemDoneEvent(tc.OutputIdx, finalItem, seq))
	}

	// === 1. response.created ===
	seq := state.NextSeq()
	emit(buildResponsesCreatedEvent(responseID, model, seq))

	// === 2. response.in_progress ===
	seq = state.NextSeq()
	emit(buildResponseInProgressEvent(responseID, model, seq))

	for scanner.Scan() {
		line := scanner.Text()

		select {
		case <-c.Request.Context().Done():
			return nil
		default:
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			currentData.WriteString(data)
		} else if line == "" && currentData.Len() > 0 {
			chunk := []byte(currentData.String())
			currentData.Reset()

			// 解析 chunk 提取 finish_reason 和 usage
			if gjson.ValidBytes(chunk) {
				choices := gjson.GetBytes(chunk, "choices")
				if choices.IsArray() {
					choices.ForEach(func(_, choice gjson.Result) bool {
						if fr := choice.Get("finish_reason").String(); fr != "" {
							state.FinishReason = fr
						}
						return true
					})
				}
				if usage := gjson.GetBytes(chunk, "usage"); usage.Exists() {
					lastUsageData = usage.Raw
					state.Usage = &MimoUsageData{
						InputTokens:  usage.Get("prompt_tokens").Int(),
						OutputTokens: usage.Get("completion_tokens").Int(),
						TotalTokens:  usage.Get("total_tokens").Int(),
					}
				}
			}

			// 检测当前 chunk 的内容类型
			hasReasoning := false
			hasContent := false
			hasToolCalls := false
			if gjson.ValidBytes(chunk) {
				choices := gjson.GetBytes(chunk, "choices")
				if choices.IsArray() {
					choices.ForEach(func(_, choice gjson.Result) bool {
						delta := choice.Get("delta")
						if rc := delta.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
							hasReasoning = true
						}
						if ct := delta.Get("content"); ct.Exists() && ct.String() != "" {
							hasContent = true
						}
						if tc := delta.Get("tool_calls"); tc.IsArray() {
							hasToolCalls = true
						}
						return true
					})
				}
			}

			// === 打开 reasoning item（第一次收到 reasoning 时）===
			if hasReasoning && !reasoningItemOpened {
				reasoningItemOpened = true
				itemID := "rs_" + responseID
				seq = state.NextSeq()
				emit(buildResponseOutputItemAddedEvent(responseID, itemID, "reasoning", state.OutputIndex, seq))
				// reasoning_summary_part.added
				seq = state.NextSeq()
				emit(buildResponsesReasoningSummaryPartAddedEvent(responseID, state.OutputIndex, seq))
			}

			// === 打开 output item（第一次收到 content 时）===
			if hasContent && !outputItemOpened {
				// 如果 reasoning item 打开着，先关闭它
				closeReasoningItem()

				outputItemOpened = true
				itemID := "msg_" + responseID
				seq = state.NextSeq()
				emit(buildResponseOutputItemAddedEvent(responseID, itemID, "message", state.OutputIndex, seq))
				seq = state.NextSeq()
				emit(buildResponseContentPartAddedEvent("output_text", state.OutputIndex, 0, seq))
			}

			// === 处理 tool_calls: 按 mimo2codex 方式流式处理 ===
			if hasToolCalls {
				// 先关闭 reasoning（如果有）
				if reasoningItemOpened {
					closeReasoningItem()
				}

				choices := gjson.GetBytes(chunk, "choices")
				if choices.IsArray() {
					choices.ForEach(func(_, choice gjson.Result) bool {
						delta := choice.Get("delta")
						toolCalls := delta.Get("tool_calls")
						if !toolCalls.IsArray() {
							return true
						}
						toolCalls.ForEach(func(_, tc gjson.Result) bool {
							tcIndex := int(tc.Get("index").Int())
							tcState, exists := state.FunctionCallSeq[tcIndex]
							if !exists {
								// 首次发现此 tool call: 发送 output_item.added
								tcName := tc.Get("function.name").String()
								tcCallID := tc.Get("id").String()
								itemID := fmt.Sprintf("fc_%d_%s", tcIndex, responseID)
								if tcCallID == "" {
									tcCallID = fmt.Sprintf("call_%s_%d", responseID, tcIndex)
								}
								outputIdx := state.OutputIndex
								state.OutputIndex++

								tcState = &MimoStreamFunctionCallState{
									ItemID:    itemID,
									CallID:    tcCallID,
									Name:      tcName,
									OutputIdx: outputIdx,
									Opened:    true,
								}
								state.FunctionCallSeq[tcIndex] = tcState

								seq = state.NextSeq()
								emit(buildResponsesFunctionCallAddedEvent(responseID, itemID, tcCallID, tcName, outputIdx, seq))
							} else if tc.Get("function.name").String() != "" && tcState.Name == "" {
								tcState.Name = tc.Get("function.name").String()
							}

							// 发送 arguments delta
							argsDelta := tc.Get("function.arguments").String()
							if argsDelta != "" {
								tcState.ArgsBuffer += argsDelta
								seq = state.NextSeq()
								emit(buildResponsesFunctionCallArgumentsDeltaEvent(tcState.ItemID, argsDelta, tcState.OutputIdx, seq))
							}

							return true
						})
						return true
					})
				}
			}

			// === 翻译并发送 reasoning/content delta 事件 ===
			if gjson.ValidBytes(chunk) {
				choices := gjson.GetBytes(chunk, "choices")
				if choices.IsArray() {
					choices.ForEach(func(_, choice gjson.Result) bool {
						delta := choice.Get("delta")

						// reasoning_content delta
						if rc := delta.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
							state.ReasoningText += rc.String()
							state.HasReasoning = true
							seq = state.NextSeq()
							emit(buildResponsesReasoningDelta(rc.String(), responseID, seq))
						}

						// content delta
						if ct := delta.Get("content"); ct.Exists() && ct.String() != "" {
							state.OutputText += ct.String()
							state.HasOutput = true
							seq = state.NextSeq()
							emit(buildResponsesTextDelta(ct.String(), responseID, seq))
						}

						return true
					})
				}
			}
		}
	}

	// 处理最后的数据
	if currentData.Len() > 0 {
		chunk := []byte(currentData.String())
		if gjson.ValidBytes(chunk) {
			choices := gjson.GetBytes(chunk, "choices")
			if choices.IsArray() {
				choices.ForEach(func(_, choice gjson.Result) bool {
					if fr := choice.Get("finish_reason").String(); fr != "" {
						state.FinishReason = fr
					}
					return true
				})
			}
		}
	}

	// === 关闭所有打开的 items ===

	// 关闭所有未关闭的 function call items
	for idx := 0; ; idx++ {
		tc, ok := state.FunctionCallSeq[idx]
		if !ok {
			break
		}
		if tc.Opened {
			closeFunctionCallItem(idx)
		}
	}

	// 关闭 reasoning item（纯 reasoning 响应或 reasoning 后无 content）
	closeReasoningItem()

	// 关闭 output item
	closeOutputItem()

	// usage 事件
	if lastUsageData != "" {
		usageBytes := []byte(lastUsageData)
		var usageJSON gjson.Result
		json.Unmarshal(usageBytes, &usageJSON)
		seq = state.NextSeq()
		emit(buildResponsesUsageEvent(usageJSON, responseID, seq))

		// 填充 usageResult（兼容 prompt_tokens 和 input_tokens 两种格式）
		usageResult.InputTokens = gjson.GetBytes(usageBytes, "prompt_tokens").Int()
		if usageResult.InputTokens == 0 {
			usageResult.InputTokens = gjson.GetBytes(usageBytes, "input_tokens").Int()
		}
		usageResult.OutputTokens = gjson.GetBytes(usageBytes, "completion_tokens").Int()
		if usageResult.OutputTokens == 0 {
			usageResult.OutputTokens = gjson.GetBytes(usageBytes, "output_tokens").Int()
		}
		usageResult.TotalTokens = gjson.GetBytes(usageBytes, "total_tokens").Int()
		if usageResult.TotalTokens == 0 {
			usageResult.TotalTokens = usageResult.InputTokens + usageResult.OutputTokens
		}
		// 提取 cached_tokens 和 reasoning_tokens
		usageResult.CachedTokens = gjson.GetBytes(usageBytes, "prompt_tokens_details.cached_tokens").Int()
		usageResult.ReasoningTokens = gjson.GetBytes(usageBytes, "completion_tokens_details.reasoning_tokens").Int()
		log.Printf("[MIMO-USAGE] input=%d, output=%d, total=%d, cached=%d, reasoning=%d", usageResult.InputTokens, usageResult.OutputTokens, usageResult.TotalTokens, usageResult.CachedTokens, usageResult.ReasoningTokens)
	} else {
		log.Printf("[MIMO-USAGE] lastUsageData is EMPTY")
	}

	// response.completed
	seq = state.NextSeq()
	completedEvent := buildResponsesCompletedEventWithDataWithSeq(responseID, model, &state.ReasoningText, &state.OutputText, lastUsageData, seq)
	emit(completedEvent)

	return usageResult
}

// handleMimoNonStreamResponse 处理 MiMo 非流式响应，翻译为 Responses 格式
func handleMimoNonStreamResponse(c *gin.Context, body io.Reader, responseID string) *MimoUsageResult {
	usageResult := &MimoUsageResult{}

	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to read response: %v", err),
		})
		return nil
	}

	// 从原始 body 中提取 usage 数据（兼容两种格式）
	if usage := gjson.GetBytes(bodyBytes, "usage"); usage.Exists() {
		usageResult.InputTokens = usage.Get("prompt_tokens").Int()
		if usageResult.InputTokens == 0 {
			usageResult.InputTokens = usage.Get("input_tokens").Int()
		}
		usageResult.OutputTokens = usage.Get("completion_tokens").Int()
		if usageResult.OutputTokens == 0 {
			usageResult.OutputTokens = usage.Get("output_tokens").Int()
		}
		usageResult.TotalTokens = usage.Get("total_tokens").Int()
		if usageResult.TotalTokens == 0 {
			usageResult.TotalTokens = usageResult.InputTokens + usageResult.OutputTokens
		}
	}

	// 翻译为 Responses 格式
	events, err := ChatChunkToResponsesEvent(bodyBytes, responseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to translate response: %v", err),
		})
		return nil
	}

	// 对于非流式响应，提取 response.completed 事件中的 response 对象
	for _, event := range events {
		if strings.Contains(event, "response.completed") {
			parts := strings.SplitN(event, "data: ", 2)
			if len(parts) == 2 {
				c.Header("Content-Type", "application/json")
				c.String(http.StatusOK, parts[1])
				return usageResult
			}
		}
	}

	// 如果没有找到 completed 事件，返回原始翻译结果
	if len(events) > 0 {
		c.Header("Content-Type", "text/event-stream")
		for _, event := range events {
			fmt.Fprintf(c.Writer, "%s\n\n", event)
		}
		return usageResult
	}

	c.JSON(http.StatusInternalServerError, map[string]string{
		"error": "empty response from mimo upstream",
	})
	return nil
}

// handleMimoPassthroughStream 透传 MiMo SSE 流（用于 Chat Completions API）
func handleMimoPassthroughStream(c *gin.Context, body io.Reader) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		io.Copy(c.Writer, body)
		return
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(c.Writer, "%s\n", line)
		if line == "" {
			flusher.Flush()
		}
	}
}

// ==================== 辅助函数 ====================

// buildResponsesCompletedEventWithDataWithSeq 构建带序列号的 response.completed 事件
func buildResponsesCompletedEventWithDataWithSeq(responseID, model string, reasoningText, outputText *string, usageJSON string, seq int) string {
	var outputItems []any

	// reasoning item
	if reasoningText != nil && *reasoningText != "" {
		outputItems = append(outputItems, map[string]any{
			"type":              "reasoning",
			"id":                "rs_" + responseID,
			"summary":           []map[string]any{{"type": "summary_text", "text": *reasoningText}},
			"encrypted_content": *reasoningText,
			"status":            "completed",
		})
	}

	// message item
	if outputText != nil && *outputText != "" {
		outputItems = append(outputItems, map[string]any{
			"type": "message",
			"id":   "msg_" + responseID,
			"role": "assistant",
			"content": []map[string]any{
				{"type": "output_text", "text": *outputText, "annotations": []any{}},
			},
			"status": "completed",
		})
	}

	if outputItems == nil {
		outputItems = []any{}
	}

	resp := map[string]any{
		"id":               responseID,
		"object":           "response",
		"created_at":       0,
		"status":           "completed",
		"model":            model,
		"output":           outputItems,
		"instructions":     "",
		"tools":            []any{},
		"tool_choice":      "auto",
		"temperature":      nil,
		"max_output_tokens": nil,
		"top_p":            nil,
		"parallel_tool_calls": true,
	}

	if usageJSON != "" {
		var usageData map[string]any
		if err := json.Unmarshal([]byte(usageJSON), &usageData); err == nil {
			resp["usage"] = convertUsageToResponsesFormat(usageData)
		}
	} else {
		resp["usage"] = map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		}
	}

	event := map[string]any{
		"type":            "response.completed",
		"response":        resp,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.completed\ndata: " + string(data)
}

func buildResponsesCreatedEvent(responseID, model string, seq int) string {
	resp := map[string]any{
		"id":         responseID,
		"object":     "response",
		"status":     "in_progress",
		"model":      model,
		"output":     []any{},
	}
	event := map[string]any{
		"type":            "response.created",
		"response":        resp,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.created\ndata: " + string(data)
}

// ApplyAccountModelMapping 应用账号级别模型映射
// mapping 格式: {"gpt-4o": "mimo-v2.5-pro", "o3-mini": "mimo-v2-flash"}
func ApplyAccountModelMapping(model string, mappingJSON string) string {
	if mappingJSON == "" || mappingJSON == "{}" {
		return model
	}

	var mapping map[string]string
	if err := json.Unmarshal([]byte(mappingJSON), &mapping); err != nil {
		return model
	}

	if mapped, ok := mapping[model]; ok {
		return mapped
	}

	return model
}

// ==================== Codex 工具输出检测 ====================

// checkCodexComplete 检查 Codex 是否完成了任务
// 通过检测特定的函数调用来判断
func checkCodexComplete(chunkData []byte) bool {
	if !gjson.ValidBytes(chunkData) {
		return false
	}

	choices := gjson.GetBytes(chunkData, "choices")
	if !choices.IsArray() {
		return false
	}

	complete := false
	choices.ForEach(func(_, choice gjson.Result) bool {
		if choice.Get("finish_reason").String() != "" {
			complete = true
			return false // 停止遍历
		}
		return true
	})

	return complete
}

// HandleMimoUpstream 处理 MiMo 上游请求的统一分发
// 根据请求路径判断是 Responses API 还是 Chat Completions API
func HandleMimoUpstream(c *gin.Context, reqInfo *RequestContext, cfg *MimoUpstreamConfig) {
	path := c.Request.URL.Path

	if strings.Contains(path, "/responses") {
		HandleMimoResponsesAPI(c, reqInfo.Body, cfg)
	} else if strings.Contains(path, "/chat/completions") {
		HandleMimoChatCompletionsAPI(c, reqInfo.Body, cfg)
	} else {
		// 默认走 Responses API
		HandleMimoResponsesAPI(c, reqInfo.Body, cfg)
	}
}

// RequestContext 请求上下文信息
type RequestContext struct {
	Body     []byte
	Stream   bool
	Model    string
	IsCodex  bool
}

// DetermineUpstreamType 判断请求应该路由到哪个上游
// 返回 "mimo", "codex", "openai"
func DetermineUpstreamType(model string, accountUpstreamType string) string {
	// 优先使用账号配置的上游类型
	if accountUpstreamType != "" {
		return strings.ToLower(strings.TrimSpace(accountUpstreamType))
	}

	// 根据模型名称判断
	if IsMimoModel(model) {
		return UpstreamTypeMimo
	}

	// 默认走 Codex
	return "codex"
}

// HandleUpstreamRequest 统一的上游请求处理入口
func HandleUpstreamRequest(c *gin.Context, reqBody []byte, upstreamType string, cfg *MimoUpstreamConfig) {
	switch upstreamType {
	case UpstreamTypeMimo:
		HandleMimoUpstream(c, &RequestContext{Body: reqBody}, cfg)
	default:
		// 默认走原始 Codex 处理
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unsupported upstream type: %s", upstreamType),
		})
	}
}