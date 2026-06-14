package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== MiMo 翻译器 ====================
// 将 OpenAI Responses API 请求翻译为 MiMo Chat Completions API
// 移植自 mimo2codex/src/translate/reqToChat.ts 和 respToResponses.ts

// MimoChatRequest MiMo Chat Completions 请求格式
type MimoChatRequest struct {
	Model               string            `json:"model"`
	Messages            []MimoChatMessage `json:"messages"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *MimoStreamOpts   `json:"stream_options,omitempty"`
	Tools               []MimoChatTool    `json:"tools,omitempty"`
	ToolChoice          any               `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Thinking            *MimoThinking     `json:"thinking,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
}

type MimoStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type MimoThinking struct {
	Type string `json:"type"` // "enabled" | "disabled" | "auto"
}

type MimoChatMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"` // string 或 []MimoContentPart
	ToolCalls        []MimoToolCall   `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type MimoContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
}

type MimoToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type MimoChatTool struct {
	Type     string         `json:"type"`
	Function *MimoToolFunc  `json:"function,omitempty"`
}

type MimoToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// MimoWebSearchTool MiMo 原生 web_search 工具
type MimoWebSearchTool struct {
	Type string `json:"type"` // "web_search"
}

// ==================== 输入聚合状态 ====================
// 移植自 mimo2codex/src/translate/reqToChat.ts 的 AssemblyState 机制
// 将多个 Responses input items 聚合为符合 Chat Completions 规范的消息序列

// AssemblyState 用于聚合多个 input items 为单条 assistant 消息
type AssemblyState struct {
	pendingReasoning     string         // 累积的推理内容
	pendingToolCalls     []MimoToolCall // 累积的 tool_calls
	pendingAssistantText string         // 累积的 assistant 文本
	hasPendingAssistant  bool           // 是否有待处理的 assistant 状态
}

// assemblyFlushAssistant 将当前累积的 assistant 状态 flush 为一条消息
// 关键规则:
//   - tool_calls 存在时不带 content 字段（DeepSeek V4 兼容）
//   - reasoning_content 放在同一消息中
//   - 纯推理回合需要空 content
func assemblyFlushAssistant(messages *[]MimoChatMessage, state *AssemblyState) {
	hasReasoning := state.pendingReasoning != ""
	hasTools := len(state.pendingToolCalls) > 0
	hasText := state.pendingAssistantText != ""
	if !hasReasoning && !hasTools && !hasText {
		return
	}

	msg := MimoChatMessage{Role: "assistant"}

	if hasText {
		msg.Content = state.pendingAssistantText
	} else if !hasTools {
		// 纯推理回合: 需要空 content 以满足 "content 或 tool_calls 必须存在"
		msg.Content = ""
	}
	// tool_calls 存在时不设置 content 字段（DeepSeek V4 兼容 issue #29）
	if hasTools {
		msg.ToolCalls = state.pendingToolCalls
	}
	if hasReasoning {
		msg.ReasoningContent = state.pendingReasoning
	}

	*messages = append(*messages, msg)

	// 重置状态
	state.pendingReasoning = ""
	state.pendingToolCalls = nil
	state.pendingAssistantText = ""
	state.hasPendingAssistant = false
}

// ==================== Responses → Chat Completions 翻译 ====================

// ResponsesToMimoChat 将 OpenAI Responses API 请求翻译为 MiMo Chat Completions 请求
func ResponsesToMimoChat(responsesBody []byte, isTokenPlan bool) ([]byte, string, error) {
	if !gjson.ValidBytes(responsesBody) {
		return nil, "", fmt.Errorf("invalid JSON body")
	}

	model := NormalizeMimoModelID(strings.TrimSpace(gjson.GetBytes(responsesBody, "model").String()))
	if model == "" {
		model = MimoDefaultModel
	}

	chat := MimoChatRequest{
		Model:  model,
		Stream: gjson.GetBytes(responsesBody, "stream").Bool(),
	}

	// 流式选项
	if chat.Stream {
		chat.StreamOptions = &MimoStreamOpts{IncludeUsage: true}
	}

	// 翻译 instructions → system message
	if instructions := gjson.GetBytes(responsesBody, "instructions").String(); instructions != "" {
		chat.Messages = append(chat.Messages, MimoChatMessage{
			Role:    "system",
			Content: instructions,
		})
	}

	// 翻译 input → messages
	input := gjson.GetBytes(responsesBody, "input")
	if input.Exists() {
		messages := translateResponsesInputToMessages(input, model)
		chat.Messages = append(chat.Messages, messages...)
	}

	// 翻译 tools
	tools := gjson.GetBytes(responsesBody, "tools")
	if tools.Exists() {
		chatTools, hasWebSearch := translateResponsesTools(tools, isTokenPlan)
		if len(chatTools) > 0 {
			chat.Tools = chatTools
		}

		// Token Plan 账号 + 有 web_search + 搜索已启用 = 自动搜索注入
		if hasWebSearch && isTokenPlan && IsSearchEnabled() {
			// 提取搜索查询
			query := ExtractSearchQuery(chat.Messages)
			if query != "" {
				slog.Info("auto search triggered", "query", query, "provider", GetSearchProvider())

				// 执行搜索
				searchResults, err := PerformSearch(query, 5)
				if err != nil {
					slog.Warn("auto search failed", "error", err)
				} else if searchResults != nil && len(searchResults.Results) > 0 {
					// 注入搜索结果到消息
					formattedResults := FormatSearchResultsForPrompt(searchResults)
					chat.Messages = InjectSearchResults(chat.Messages, formattedResults)
					slog.Info("search results injected", "count", len(searchResults.Results))
				}

				// 移除 web_search 工具（避免 MiMo 400 错误）
				chat.Tools = RemoveWebSearchTool(chat.Tools)
			}
		}
	}

	// 翻译 tool_choice
	if toolChoice := gjson.GetBytes(responsesBody, "tool_choice"); toolChoice.Exists() {
		chat.ToolChoice = translateToolChoice(toolChoice)
	}

	// 翻译 parallel_tool_calls
	if pt := gjson.GetBytes(responsesBody, "parallel_tool_calls"); pt.Exists() {
		val := pt.Bool()
		chat.ParallelToolCalls = &val
	}

	// 翻译 temperature
	if temp := gjson.GetBytes(responsesBody, "temperature"); temp.Exists() && temp.Type != gjson.Null {
		val := temp.Float()
		chat.Temperature = &val
	}

	// 翻译 max_output_tokens → max_completion_tokens
	if maxTokens := gjson.GetBytes(responsesBody, "max_output_tokens"); maxTokens.Exists() && maxTokens.Type != gjson.Null {
		val := int(maxTokens.Int())
		chat.MaxCompletionTokens = &val
	}

	// 翻译 reasoning.effort → reasoning_effort
	if effort := gjson.GetBytes(responsesBody, "reasoning.effort"); effort.Exists() {
		chat.ReasoningEffort = normalizeReasoningEffort(effort.String())
	}

	// MiMo 特殊处理: 注入 thinking 默认值
	normalizeMimoThinking(&chat, model)

	// MiMo 特殊处理: thinking 模式下移除 temperature
	if MimoThinkingFixesTemperature[model] && chat.Thinking != nil && chat.Thinking.Type == "enabled" {
		chat.Temperature = nil
	}

	// MiMo 特殊处理: tool_choice 非 "auto" 时移除
	if chat.ToolChoice != nil {
		if tc, ok := chat.ToolChoice.(string); ok && tc != "auto" {
			chat.ToolChoice = nil
		}
	}

	// 清理 reasoning_effort: thinking disabled 时不需要
	if chat.Thinking != nil && chat.Thinking.Type == "disabled" && chat.ReasoningEffort == "none" {
		chat.ReasoningEffort = ""
	}

	result, err := json.Marshal(chat)
	return result, model, err
}

// translateResponsesInputToMessages 翻译 Responses input 到 Chat messages
// 使用 AssemblyState 聚合多个 input items 为符合 Chat Completions 规范的消息序列
func translateResponsesInputToMessages(input gjson.Result, model string) []MimoChatMessage {
	if input.Type == gjson.String {
		// 简单字符串 input → user message
		return []MimoChatMessage{
			{Role: "user", Content: input.String()},
		}
	}

	if !input.IsArray() {
		return nil
	}

	messages := make([]MimoChatMessage, 0)
	supportsImages := MimoModelSupportsImages(model)
	state := &AssemblyState{}

	input.ForEach(func(_, item gjson.Result) bool {
		// 兼容 Chat-Completions 形状的探测请求: {role, content} 无 type 字段
		if !item.Get("type").Exists() {
			if role := item.Get("role").String(); role != "" {
				content := item.Get("content")
				text := ""
				if content.Type == gjson.String {
					text = content.String()
				}
				// 构造一个等价的 message item
				if role == "assistant" {
					state.pendingAssistantText = text
					state.hasPendingAssistant = true
				} else {
					assemblyFlushAssistant(&messages, state)
					messages = append(messages, MimoChatMessage{
						Role:    role,
						Content: text,
					})
				}
				return true
			}
		}

		itemType := item.Get("type").String()

		switch itemType {
		case "message":
			role := item.Get("role").String()
			if role == "developer" {
				// developer 消息通过 instructions 处理，跳过
				return true
			}
			if role == "assistant" {
				// Buffer assistant text into pending, 不立即 flush —— 后续的
				// function_call items 需要合并到同一条 assistant 消息中
				if state.pendingAssistantText != "" {
					assemblyFlushAssistant(&messages, state)
				}
				content := translateResponsesInputItemContent(item, supportsImages)
				text := ""
				if s, ok := content.(string); ok {
					text = s
				}
				state.pendingAssistantText = text
				state.hasPendingAssistant = true
			} else {
				// user / system / 其他角色: 先 flush assistant 状态，然后添加消息
				assemblyFlushAssistant(&messages, state)
				// role=system 保持为 system（已由 instructions 处理，但直接传入时也接受）
				messages = append(messages, MimoChatMessage{
					Role:    role,
					Content: translateResponsesInputItemContent(item, supportsImages),
				})
			}

		case "reasoning":
			// reasoning item: 优先使用 encrypted_content（Codex 跨轮次回传必需）
			// 累积到 pendingReasoning，不立即 flush
			text := extractReasoningText(item)
			if text == "" {
				return true
			}
			// 如果已有 pending tool_calls 或 assistant text，合并到同一消息
			if len(state.pendingToolCalls) > 0 || state.pendingAssistantText != "" {
				if state.pendingReasoning != "" {
					state.pendingReasoning += text
				} else {
					state.pendingReasoning = text
				}
			} else {
				assemblyFlushAssistant(&messages, state)
				state.pendingReasoning = text
			}

		case "function_call":
			// function_call item: 累积到 pendingToolCalls，不立即 flush
			state.pendingToolCalls = append(state.pendingToolCalls, MimoToolCall{
				ID:   item.Get("call_id").String(),
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      item.Get("name").String(),
					Arguments: sanitizeFunctionCallArguments(item.Get("arguments").String(), item.Get("name").String()),
				},
			})

		case "function_call_output":
			// function_call_output: 先 flush 当前 assistant 状态，然后作为 tool 消息
			assemblyFlushAssistant(&messages, state)
			messages = append(messages, MimoChatMessage{
				Role:       "tool",
				ToolCallID: item.Get("call_id").String(),
				Content:    extractOutputText(item.Get("output")),
			})
		}

		return true
	})

	// flush 最后剩余的 assistant 状态
	assemblyFlushAssistant(&messages, state)

	// 后处理: 清理孤立 tool 消息 + 补全缺失 tool output
	removeOrphanToolMessages(&messages)
	ensureToolCallHaveOutputs(&messages)

	return messages
}

// translateResponsesInputItemContent 翻译 input item 的 content 字段
func translateResponsesInputItemContent(item gjson.Result, supportsImages bool) any {
	content := item.Get("content")
	if !content.Exists() {
		return ""
	}

	if content.Type == gjson.String {
		return content.String()
	}

	if content.IsArray() {
		parts := translateContentParts(content, supportsImages)
		if len(parts) == 0 {
			return ""
		}
		if len(parts) == 1 && parts[0].Type == "text" {
			return parts[0].Text
		}
		return parts
	}

	return content.String()
}

// translateContentParts 翻译内容部分
func translateContentParts(content gjson.Result, supportsImages bool) []MimoContentPart {
	var parts []MimoContentPart

	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "input_text", "output_text":
			text := part.Get("text").String()
			if text != "" {
				parts = append(parts, MimoContentPart{Type: "text", Text: text})
			}
		case "input_image":
			if supportsImages {
				parts = append(parts, MimoContentPart{
					Type: "image_url",
					ImageURL: &struct {
						URL    string `json:"url"`
						Detail string `json:"detail,omitempty"`
					}{
						URL:    part.Get("image_url").String(),
						Detail: part.Get("detail").String(),
					},
				})
			}
			// 不支持图片时静默丢弃
		}
		return true
	})

	return parts
}

// extractOutputText 提取输出文本
func extractOutputText(output gjson.Result) string {
	if output.Type == gjson.String {
		return output.String()
	}
	if output.IsArray() {
		var texts []string
		output.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "output_text" || part.Get("type").String() == "input_text" {
				if text := part.Get("text").String(); text != "" {
					texts = append(texts, text)
				}
			}
			return true
		})
		return strings.Join(texts, "\n")
	}
	return output.String()
}

// extractReasoningText 提取 reasoning 文本
func extractReasoningText(item gjson.Result) string {
	// 优先使用 encrypted_content
	if ec := item.Get("encrypted_content"); ec.Exists() && ec.String() != "" {
		return ec.String()
	}
	// 回退到 summary
	summary := item.Get("summary")
	if summary.IsArray() {
		var texts []string
		summary.ForEach(func(_, s gjson.Result) bool {
			if s.Get("type").String() == "summary_text" {
				texts = append(texts, s.Get("text").String())
			}
			return true
		})
		return strings.Join(texts, "")
	}
	return ""
}

// ==================== 输入后处理函数 ====================

// sanitizeFunctionCallArguments 校验 tool call arguments 是否为合法 JSON
// 如果不合法（不完整 JSON），替换为 "{}"
// 不删除整个 tool_call —— 保持消息对结构
func sanitizeFunctionCallArguments(raw string, name string) string {
	if raw == "" {
		return "{}"
	}
	if json.Valid([]byte(raw)) {
		return raw
	}
	slog.Warn("salvaging malformed tool_call arguments",
		"name", name,
		"len", len(raw),
		"preview", truncateString(raw, 80),
	)
	return "{}"
}

// sanitizeToolCallArgumentsOutput 校验响应方向的 tool call arguments
func sanitizeToolCallArgumentsOutput(raw string, name, callID, finishReason string) string {
	if raw == "" {
		return raw
	}
	if json.Valid([]byte(raw)) {
		return raw
	}
	reason := "upstream returned malformed JSON in arguments"
	if finishReason == "length" {
		reason = "response truncated by length limit"
	}
	slog.Warn("tool_call arguments not valid JSON; salvaged to \"{}\"",
		"name", name,
		"call_id", callID,
		"len", len(raw),
		"finish_reason", finishReason,
		"cause", reason,
		"preview", truncateString(raw, 80),
	)
	return "{}"
}

// truncateString 截断字符串用于日志
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// removeOrphanToolMessages 清理孤立的 tool 消息
// 当 Codex session 状态失同步时（用户中断并行工具调用、crash 后部分回放等），
// 可能出现 role=tool 的消息但其 tool_call_id 在前面的 assistant 消息中找不到匹配。
// DeepSeek V4 会拒绝这些消息并返回 400。
func removeOrphanToolMessages(messages *[]MimoChatMessage) {
	var validIDs map[string]bool
	i := 0
	for i < len(*messages) {
		m := &(*messages)[i]
		if m.Role == "assistant" {
			if len(m.ToolCalls) > 0 {
				validIDs = make(map[string]bool)
				for _, tc := range m.ToolCalls {
					if tc.ID != "" {
						validIDs[tc.ID] = true
					}
				}
			} else {
				validIDs = nil
			}
			i++
		} else if m.Role == "tool" {
			if validIDs != nil && m.ToolCallID != "" && validIDs[m.ToolCallID] {
				i++
			} else {
				slog.Warn("dropped orphan tool message",
					"tool_call_id", m.ToolCallID,
				)
				// splice: 不递增 i，下一个元素已经移到当前位置
				*messages = append((*messages)[:i], (*messages)[i+1:]...)
			}
		} else {
			// user / system / 其他: 重置 tool 接收窗口
			validIDs = nil
			i++
		}
	}
}

// ensureToolCallHaveOutputs 确保每个带 tool_calls 的 assistant 消息后面
// 都有对应的 role=tool 消息。如果缺少，添加占位消息。
func ensureToolCallHaveOutputs(messages *[]MimoChatMessage) {
	for i := 0; i < len(*messages); i++ {
		m := &(*messages)[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}

		// 收集后面连续的 tool 消息中的 tool_call_id
		seen := make(map[string]bool)
		j := i + 1
		for j < len(*messages) && (*messages)[j].Role == "tool" {
			if (*messages)[j].ToolCallID != "" {
				seen[(*messages)[j].ToolCallID] = true
			}
			j++
		}

		// 查找缺失的 tool_call_id
		var missing []string
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !seen[tc.ID] {
				missing = append(missing, tc.ID)
			}
		}
		if len(missing) == 0 {
			continue
		}

		// 在位置 j 插入占位 tool 消息
		placeholders := make([]MimoChatMessage, 0, len(missing))
		for _, id := range missing {
			placeholders = append(placeholders, MimoChatMessage{
				Role:       "tool",
				ToolCallID: id,
				Content:    "[tool output missing]",
			})
		}

		// 插入占位消息
		*messages = append((*messages)[:j], append(placeholders, (*messages)[j:]...)...)
		// 跳过刚插入的占位消息，避免重复扫描
		i = j + len(placeholders) - 1
	}
}

// translateResponsesTools 翻译 Responses tools 到 Chat tools
func translateResponsesTools(tools gjson.Result, isTokenPlan bool) ([]MimoChatTool, bool) {
	var chatTools []MimoChatTool
	hasWebSearch := false

	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := tool.Get("type").String()

		switch toolType {
		case "function":
			name := tool.Get("name").String()
			if name == "" {
				return true // 跳过无名工具
			}
			chatTool := MimoChatTool{
				Type: "function",
				Function: &MimoToolFunc{
					Name:        name,
					Description: tool.Get("description").String(),
				},
			}
			if params := tool.Get("parameters"); params.Exists() && params.Raw != "" {
				chatTool.Function.Parameters = json.RawMessage(params.Raw)
			}
			chatTools = append(chatTools, chatTool)

		case "local_shell":
			// Codex 的 local_shell → MiMo 的 shell function
			chatTools = append(chatTools, MimoChatTool{
				Type: "function",
				Function: &MimoToolFunc{
					Name:        "shell",
					Description: "Execute a shell command on the local machine. Returns stdout, stderr and exit code.",
					Parameters: json.RawMessage(`{
						"type": "object",
						"properties": {
							"command": {
								"type": "array",
								"items": {"type": "string"},
								"description": "Argv array, e.g. [\"ls\", \"-la\"]"
							},
							"workdir": {
								"type": "string",
								"description": "Working directory (optional)"
							},
							"timeout_ms": {
								"type": "number",
								"description": "Timeout in milliseconds (optional)"
							}
						},
						"required": ["command"]
					}`),
				},
			})

		case "web_search", "web_search_preview":
			// Token Plan 账号默认没有激活 Web Search 插件
			// 如果发送 web_search，MiMo 会返回 400: "webSearchEnabled is false"
			// 用户需要在 https://platform.xiaomimimo.com/#/console/plugin 激活
			// 这里默认移除以避免 400 错误，但可通过环境变量强制启用
			forceWebSearch := os.Getenv("MIMO_FORCE_WEB_SEARCH") == "true"
			if !isTokenPlan || forceWebSearch {
				hasWebSearch = true
				chatTools = append(chatTools, MimoChatTool{Type: "web_search"})
			} else {
				slog.Debug("web_search skipped for token-plan account (set MIMO_FORCE_WEB_SEARCH=true to override)")
			}

		case "tool_search":
			// Codex Desktop 的 tool_search builtin -- 延迟工具发现机制
			// 翻译为普通 function tool，name="tool_search"
			toolSearch := MimoChatTool{
				Type: "function",
				Function: &MimoToolFunc{
					Name: "tool_search",
				},
			}
			if desc := tool.Get("description").String(); desc != "" {
				toolSearch.Function.Description = desc
			}
			if params := tool.Get("parameters"); params.Exists() && params.Raw != "" {
				toolSearch.Function.Parameters = json.RawMessage(params.Raw)
			}
			chatTools = append(chatTools, toolSearch)

		case "code_interpreter", "file_search", "image_generation",
			"computer_use_preview", "computer_use":
			// 这些工具 MiMo 不支持，静默丢弃

		case "namespace":
			// 递归处理 namespace 内的工具
			if nested := tool.Get("tools"); nested.Exists() {
				nestedTools, _ := translateResponsesTools(nested, isTokenPlan)
				chatTools = append(chatTools, nestedTools...)
			}

		case "mcp":
			// MCP 工具 MiMo 不支持，静默丢弃

		case "custom":
			// custom tool → function tool
			name := tool.Get("name").String()
			if name != "" {
				chatTools = append(chatTools, MimoChatTool{
					Type: "function",
					Function: &MimoToolFunc{
						Name:        name,
						Description: tool.Get("description").String(),
						Parameters:  json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"additionalProperties":true}`),
					},
				})
			}
		}
		return true
	})

	// 去重
	chatTools = deduplicateChatTools(chatTools)

	return chatTools, hasWebSearch
}

// deduplicateChatTools 去重工具列表
func deduplicateChatTools(tools []MimoChatTool) []MimoChatTool {
	seen := make(map[string]bool)
	var result []MimoChatTool
	for _, t := range tools {
		key := t.Type
		if t.Function != nil {
			key = "fn:" + t.Function.Name
		}
		if !seen[key] {
			seen[key] = true
			result = append(result, t)
		}
	}
	return result
}

// translateToolChoice 翻译 tool_choice
func translateToolChoice(tc gjson.Result) any {
	if tc.Type == gjson.String {
		return tc.String()
	}
	if tc.Get("type").String() == "function" {
		name := tc.Get("function.name").String()
		if name == "" {
			name = tc.Get("name").String()
		}
		if name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]string{"name": name},
			}
		}
	}
	return nil
}

// normalizeMimoThinking 注入 MiMo thinking 默认值
func normalizeMimoThinking(chat *MimoChatRequest, model string) {
	if chat.Thinking != nil {
		return // 已经设置了
	}
	// 默认禁用 thinking 的模型不注入
	if MimoThinkingDefaultDisabled[model] {
		return
	}
	// 其他模型默认启用 thinking
	chat.Thinking = &MimoThinking{Type: "enabled"}
}

// ==================== Chat Completions → Responses 翻译 ====================

// ChatChunkToResponsesEvent 将 MiMo Chat Completions 流式 chunk 翻译为 Responses SSE 事件
// 返回多个 SSE 事件行
func ChatChunkToResponsesEvent(chunk []byte, responseID string) ([]string, error) {
	if !gjson.ValidBytes(chunk) {
		return nil, nil
	}

	var events []string

	chunkType := gjson.GetBytes(chunk, "object").String()
	if chunkType == "chat.completion.chunk" {
		events = translateChatStreamChunk(chunk, responseID)
	} else if chunkType == "chat.completion" {
		// 非流式响应
		events = translateChatCompletion(chunk, responseID)
	}

	return events, nil
}

// MimoStreamState 流式响应累积状态
type MimoStreamState struct {
	ResponseID      string
	Model           string
	ReasoningText   string
	OutputText      string
	HasReasoning    bool
	HasOutput       bool
	HasToolCalls    bool
	ToolCalls       []MimoStreamToolCall
	Usage           *MimoUsageData
	FinishReason    string
	OutputIndex     int                                    // 当前 output item 索引
	SequenceNumber  int                                    // 全局序列号（从 0 开始递增）
	FunctionCallSeq map[int]*MimoStreamFunctionCallState   // index → function call state
}

// NextSeq 返回下一个序列号并递增
func (s *MimoStreamState) NextSeq() int {
	n := s.SequenceNumber
	s.SequenceNumber++
	return n
}

// MimoStreamToolCall 流式 tool call 累积
type MimoStreamToolCall struct {
	ID        string
	CallID    string
	Name      string
	Arguments string
}

// MimoStreamFunctionCallState 流式 function call 累积状态
type MimoStreamFunctionCallState struct {
	ItemID     string
	CallID     string
	Name       string
	ArgsBuffer string
	OutputIdx  int
	Opened     bool // 是否已发送 output_item.added
}

// MimoUsageData usage 数据
type MimoUsageData struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// translateChatStreamChunk 翻译流式 chunk
// 注意: 此函数现在仅用于非流式路径（ChatChunkToResponsesEvent）
// 流式路径由 handleMimoStreamResponse 直接处理
func translateChatStreamChunk(chunk []byte, responseID string) []string {
	var events []string

	choices := gjson.GetBytes(chunk, "choices")
	if !choices.IsArray() {
		// 可能是 usage-only chunk
		if usage := gjson.GetBytes(chunk, "usage"); usage.Exists() {
			events = append(events, buildResponsesUsageEvent(usage, responseID, 0))
		}
		return events
	}

	choices.ForEach(func(_, choice gjson.Result) bool {
		delta := choice.Get("delta")

		// reasoning_content → reasoning event
		if rc := delta.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
			events = append(events, buildResponsesReasoningDelta(rc.String(), responseID, 0))
		}

		// content → output_text delta
		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			events = append(events, buildResponsesTextDelta(content.String(), responseID, 0))
		}

		// tool_calls → function_call events (简化版，用于非流式路径)
		if toolCalls := delta.Get("tool_calls"); toolCalls.IsArray() {
			toolCalls.ForEach(func(_, tc gjson.Result) bool {
				callID := tc.Get("id").String()
				name := tc.Get("function.name").String()
				args := tc.Get("function.arguments").String()
				if name != "" && callID != "" {
					itemID := "fc_" + callID
					// arguments delta
					if args != "" {
						events = append(events, buildResponsesFunctionCallArgumentsDeltaEvent(itemID, args, 0, 0))
					}
				}
				return true
			})
		}

		return true
	})

	// usage event (通常在最后一个 chunk)
	if usage := gjson.GetBytes(chunk, "usage"); usage.Exists() {
		events = append(events, buildResponsesUsageEvent(usage, responseID, 0))
	}

	return events
}

// translateChatCompletion 翻译非流式响应
func translateChatCompletion(completion []byte, responseID string) []string {
	var events []string

	// 构建完整的 Responses 对象
	resp := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": gjson.GetBytes(completion, "created").Int(),
		"status":     "completed",
		"model":      gjson.GetBytes(completion, "model").String(),
		"output":     []any{},
		"usage":      nil,
	}

	// 翻译 choices
	choices := gjson.GetBytes(completion, "choices")
	if choices.IsArray() && choices.Get("0").Exists() {
		choice := choices.Get("0")
		message := choice.Get("message")

		var outputItems []any

		// reasoning_content → reasoning item
		if rc := message.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
			outputItems = append(outputItems, map[string]any{
				"type":              "reasoning",
				"id":                "rs_" + responseID,
				"summary":           []map[string]any{{"type": "summary_text", "text": rc.String()}},
				"encrypted_content": rc.String(),
				"status":            "completed",
			})
		}

		// content → message item
		if content := message.Get("content"); content.Exists() && content.String() != "" {
			outputItems = append(outputItems, map[string]any{
				"type":   "message",
				"id":     "msg_" + responseID,
				"role":   "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": content.String()},
				},
				"status": "completed",
			})
		}

		// tool_calls → function_call items
		if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() {
			toolCalls.ForEach(func(_, tc gjson.Result) bool {
				outputItems = append(outputItems, map[string]any{
					"type":     "function_call",
					"id":       "fc_" + tc.Get("id").String(),
					"call_id":  tc.Get("id").String(),
					"name":     tc.Get("function.name").String(),
					"arguments": tc.Get("function.arguments").String(),
					"status":   "completed",
				})
				return true
			})
		}

		resp["output"] = outputItems
	}

	// usage
	if usage := gjson.GetBytes(completion, "usage"); usage.Exists() {
		resp["usage"] = map[string]any{
			"input_tokens":  usage.Get("prompt_tokens").Int(),
			"output_tokens": usage.Get("completion_tokens").Int(),
			"total_tokens":  usage.Get("total_tokens").Int(),
		}
	}

	data, _ := json.Marshal(resp)
	events = append(events, "event: response.completed\ndata: "+string(data))

	return events
}

// ==================== SSE 事件构建函数 ====================

func buildResponsesTextDelta(text, responseID string, seq int) string {
	event := map[string]any{
		"type":            "response.output_text.delta",
		"item_id":         "msg_" + responseID,
		"output_index":    0,
		"content_index":   0,
		"delta":           text,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.output_text.delta\ndata: " + string(data)
}

func buildResponsesReasoningDelta(text, responseID string, seq int) string {
	event := map[string]any{
		"type":            "response.reasoning_summary_text.delta",
		"item_id":         "rs_" + responseID,
		"output_index":    0,
		"summary_index":   0,
		"delta":           text,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.reasoning_summary_text.delta\ndata: " + string(data)
}

// buildResponsesReasoningSummaryPartAddedEvent 构建 response.reasoning_summary_part.added 事件
func buildResponsesReasoningSummaryPartAddedEvent(responseID string, outputIndex int, seq int) string {
	event := map[string]any{
		"type":            "response.reasoning_summary_part.added",
		"item_id":         "rs_" + responseID,
		"output_index":    outputIndex,
		"summary_index":   0,
		"part":            map[string]any{"type": "summary_text", "text": ""},
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.reasoning_summary_part.added\ndata: " + string(data)
}

// buildResponsesReasoningSummaryPartDoneEvent 构建 response.reasoning_summary_part.done 事件
func buildResponsesReasoningSummaryPartDoneEvent(text, responseID string, outputIndex, seq int) string {
	event := map[string]any{
		"type":            "response.reasoning_summary_part.done",
		"item_id":         "rs_" + responseID,
		"output_index":    outputIndex,
		"summary_index":   0,
		"part":            map[string]any{"type": "summary_text", "text": text},
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.reasoning_summary_part.done\ndata: " + string(data)
}

// buildResponsesFunctionCallAddedEvent 构建 function_call 的 output_item.added 事件
func buildResponsesFunctionCallAddedEvent(responseID, itemID, callID, name string, outputIndex, seq int) string {
	item := map[string]any{
		"id":       itemID,
		"type":     "function_call",
		"call_id":  callID,
		"name":     name,
		"arguments": "",
		"status":   "in_progress",
	}
	event := map[string]any{
		"type":            "response.output_item.added",
		"output_index":    outputIndex,
		"item":            item,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.output_item.added\ndata: " + string(data)
}

// buildResponsesFunctionCallArgumentsDeltaEvent 构建 function_call arguments delta 事件
func buildResponsesFunctionCallArgumentsDeltaEvent(itemID string, argsDelta string, outputIndex, seq int) string {
	event := map[string]any{
		"type":            "response.function_call_arguments.delta",
		"item_id":         itemID,
		"output_index":    outputIndex,
		"delta":           argsDelta,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.function_call_arguments.delta\ndata: " + string(data)
}

// buildResponsesFunctionCallArgumentsDoneEvent 构建 function_call arguments done 事件
func buildResponsesFunctionCallArgumentsDoneEvent(itemID, args string, outputIndex, seq int) string {
	event := map[string]any{
		"type":            "response.function_call_arguments.done",
		"item_id":         itemID,
		"output_index":    outputIndex,
		"arguments":       args,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.function_call_arguments.done\ndata: " + string(data)
}

func buildResponsesUsageEvent(usage gjson.Result, responseID string, seq int) string {
	// 兼容两种格式：prompt_tokens/completion_tokens 和 input_tokens/output_tokens
	inputTokens := usage.Get("prompt_tokens").Int()
	if inputTokens == 0 {
		inputTokens = usage.Get("input_tokens").Int()
	}
	outputTokens := usage.Get("completion_tokens").Int()
	if outputTokens == 0 {
		outputTokens = usage.Get("output_tokens").Int()
	}
	totalTokens := usage.Get("total_tokens").Int()
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens
	}
	event := map[string]any{
		"type": "response.usage",
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"total_tokens":  totalTokens,
		},
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.usage\ndata: " + string(data)
}

// buildResponsesCompletedEvent 构建完整的 response.completed 事件
// 包含完整的 output items、usage、model 等所有必要字段
func buildResponsesCompletedEvent(responseID string) string {
	// 简单版本 — 用于非累积场景（如流式 chunk 中不再调用）
	return buildResponsesCompletedEventWithData(responseID, "mimo", nil, nil, "")
}

// buildResponsesCompletedEventWithData 构建包含完整数据的 response.completed 事件
func buildResponsesCompletedEventWithData(responseID, model string, reasoningText, outputText *string, usageJSON string) string {
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
		"id":          responseID,
		"object":      "response",
		"created_at":  0,
		"status":      "completed",
		"model":       model,
		"output":      outputItems,
		"instructions": "",
		"tools":       []any{},
		"tool_choice": "auto",
		"temperature": nil,
		"max_output_tokens": nil,
		"top_p":       nil,
		"parallel_tool_calls": true,
	}

	// 添加 usage — 转换 MiMo 字段名为 Responses API 格式
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
		"type":     "response.completed",
		"response": resp,
	}
	data, _ := json.Marshal(event)
	return "event: response.completed\ndata: " + string(data)
}

// convertUsageToResponsesFormat 将 MiMo/Chat Completions 的 usage 格式转换为 Responses API 格式
// prompt_tokens → input_tokens, completion_tokens → output_tokens
func convertUsageToResponsesFormat(usage map[string]any) map[string]any {
	result := map[string]any{}

	// input_tokens: 优先使用 input_tokens，回退到 prompt_tokens
	if v, ok := usage["input_tokens"]; ok {
		result["input_tokens"] = v
	} else if v, ok := usage["prompt_tokens"]; ok {
		result["input_tokens"] = v
	} else {
		result["input_tokens"] = 0
	}

	// output_tokens: 优先使用 output_tokens，回退到 completion_tokens
	if v, ok := usage["output_tokens"]; ok {
		result["output_tokens"] = v
	} else if v, ok := usage["completion_tokens"]; ok {
		result["output_tokens"] = v
	} else {
		result["output_tokens"] = 0
	}

	// total_tokens
	if v, ok := usage["total_tokens"]; ok {
		result["total_tokens"] = v
	} else {
		// json.Unmarshal 产生 float64，需要兼容处理
		var inp, out int64
		switch v := result["input_tokens"].(type) {
		case float64:
			inp = int64(v)
		case int64:
			inp = v
		case int:
			inp = int64(v)
		}
		switch v := result["output_tokens"].(type) {
		case float64:
			out = int64(v)
		case int64:
			out = v
		case int:
			out = int64(v)
		}
		result["total_tokens"] = inp + out
	}

	return result
}

// ==================== 生命周期事件构建函数 ====================

// buildResponseInProgressEvent 构建 response.in_progress 事件
func buildResponseInProgressEvent(responseID, model string, seq int) string {
	resp := map[string]any{
		"id":         responseID,
		"object":     "response",
		"status":     "in_progress",
		"model":      model,
		"output":     []any{},
	}
	event := map[string]any{
		"type":            "response.in_progress",
		"response":        resp,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.in_progress\ndata: " + string(data)
}

// buildResponseOutputItemAddedEvent 构建 response.output_item.added 事件
func buildResponseOutputItemAddedEvent(responseID, itemID, itemType string, outputIndex, seq int) string {
	var item map[string]any
	switch itemType {
	case "reasoning":
		item = map[string]any{
			"type":              "reasoning",
			"id":                itemID,
			"summary":           []any{},
			"encrypted_content": nil,
			"status":            "in_progress",
		}
	case "message":
		item = map[string]any{
			"type":    "message",
			"id":      itemID,
			"role":    "assistant",
			"content": []any{},
			"status":  "in_progress",
		}
	case "function_call":
		item = map[string]any{
			"type":   "function_call",
			"id":     itemID,
			"status": "in_progress",
		}
	default:
		item = map[string]any{"type": itemType, "id": itemID, "status": "in_progress"}
	}
	event := map[string]any{
		"type":            "response.output_item.added",
		"output_index":    outputIndex,
		"item":            item,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.output_item.added\ndata: " + string(data)
}

// buildResponseOutputItemDoneEvent 构建 response.output_item.done 事件
func buildResponseOutputItemDoneEvent(outputIndex int, itemData any, seq int) string {
	event := map[string]any{
		"type":            "response.output_item.done",
		"output_index":    outputIndex,
		"item":            itemData,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.output_item.done\ndata: " + string(data)
}

// buildResponseContentPartAddedEvent 构建 response.content_part.added 事件
func buildResponseContentPartAddedEvent(partType string, outputIndex, contentIndex, seq int) string {
	var part map[string]any
	switch partType {
	case "reasoning_summary":
		part = map[string]any{
			"type": "reasoning_summary",
		}
	case "output_text":
		part = map[string]any{
			"type":        "output_text",
			"annotations": []any{},
		}
	default:
		part = map[string]any{"type": partType}
	}
	event := map[string]any{
		"type":            "response.content_part.added",
		"output_index":    outputIndex,
		"content_index":   contentIndex,
		"part":            part,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.content_part.added\ndata: " + string(data)
}

// buildResponseContentPartDoneEvent 构建 response.content_part.done 事件
func buildResponseContentPartDoneEvent(part any, outputIndex, contentIndex, seq int) string {
	event := map[string]any{
		"type":            "response.content_part.done",
		"output_index":    outputIndex,
		"content_index":   contentIndex,
		"part":            part,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.content_part.done\ndata: " + string(data)
}

// buildResponseReasoningTextDoneEvent 构建 response.reasoning_summary_text.done 事件
func buildResponseReasoningTextDoneEvent(text, responseID string, summaryIndex, seq int) string {
	event := map[string]any{
		"type":            "response.reasoning_summary_text.done",
		"item_id":         "rs_" + responseID,
		"output_index":    0,
		"summary_index":   summaryIndex,
		"text":            text,
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.reasoning_summary_text.done\ndata: " + string(data)
}

// buildResponseOutputTextDoneEvent 构建 response.output_text.done 事件
func buildResponseOutputTextDoneEvent(text, responseID string, seq int) string {
	event := map[string]any{
		"type":            "response.output_text.done",
		"item_id":         "msg_" + responseID,
		"output_index":    0,
		"content_index":   0,
		"text":            text,
		"annotations":     []any{},
		"sequence_number": seq,
	}
	data, _ := json.Marshal(event)
	return "event: response.output_text.done\ndata: " + string(data)
}

// ==================== Chat Completions API 请求处理 ====================

// TranslateChatCompletionsToMimo 将标准 Chat Completions 请求翻译为 MiMo 格式
func TranslateChatCompletionsToMimo(chatBody []byte) ([]byte, string, error) {
	if !gjson.ValidBytes(chatBody) {
		return nil, "", fmt.Errorf("invalid JSON body")
	}

	model := NormalizeMimoModelID(strings.TrimSpace(gjson.GetBytes(chatBody, "model").String()))
	if model == "" {
		model = MimoDefaultModel
	}

	// 直接使用原始 body，只做必要的修改
	body := chatBody

	// 设置正确的模型
	var err error
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, "", err
	}

	// 注入 thinking 默认值
	if !gjson.GetBytes(body, "thinking").Exists() && !MimoThinkingDefaultDisabled[model] {
		body, _ = sjson.SetBytes(body, "thinking", map[string]string{"type": "enabled"})
	}

	// thinking 模式下移除 temperature
	if MimoThinkingFixesTemperature[model] {
		thinkingType := gjson.GetBytes(body, "thinking.type").String()
		if thinkingType == "enabled" {
			body, _ = sjson.DeleteBytes(body, "temperature")
		}
	}

	// stream_options 注入
	if gjson.GetBytes(body, "stream").Bool() && !gjson.GetBytes(body, "stream_options").Exists() {
		body, _ = sjson.SetBytes(body, "stream_options", map[string]bool{"include_usage": true})
	}

	// 检查是否有 web_search 工具
	hasWebSearch := false
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "web_search" {
				hasWebSearch = true
				return false
			}
			return true
		})
	}

	// Token Plan 账号 + 搜索已启用 = 自动搜索注入
	if hasWebSearch && IsSearchEnabled() {
		// 提取搜索查询（从最后一条 user 消息）
		query := ""
		messages := gjson.GetBytes(body, "messages")
		if messages.IsArray() {
			// 从后往前找 user 消息
			for i := len(messages.Array()) - 1; i >= 0; i-- {
				msg := messages.Array()[i]
				if msg.Get("role").String() == "user" {
					query = msg.Get("content").String()
					break
				}
			}
		}

		if query != "" {
			slog.Info("auto search triggered for chat completions", "query", query)

			// 执行搜索
			searchResults, err := PerformSearch(query, 5)
			if err != nil {
				slog.Warn("auto search failed", "error", err)
			} else if searchResults != nil && len(searchResults.Results) > 0 {
				// 注入搜索结果到 system 消息
				formattedResults := FormatSearchResultsForPrompt(searchResults)
				body = injectSearchResultsToChatBody(body, formattedResults)
				slog.Info("search results injected", "count", len(searchResults.Results))
			}

			// 移除 web_search 工具
			body = removeWebSearchFromBody(body)
		}
	}

	return body, model, nil
}

// injectSearchResultsToChatBody 将搜索结果注入到 Chat Completions body 的 system 消息中
func injectSearchResultsToChatBody(body []byte, searchResults string) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}

	// 查找并更新 system 消息
	systemIdx := -1
	messages.ForEach(func(idx gjson.Result, msg gjson.Result) bool {
		if msg.Get("role").String() == "system" {
			systemIdx = int(idx.Int())
			return false
		}
		return true
	})

	if systemIdx >= 0 {
		// 追加到现有 system 消息
		path := fmt.Sprintf("messages.%d.content", systemIdx)
		body, _ = sjson.SetBytes(body, path,
			gjson.GetBytes(body, path).String()+"\n\n"+searchResults)
	} else {
		// 插入新的 system 消息
		systemMsg := map[string]string{
			"role":    "system",
			"content": searchResults,
		}
		// 在 messages 数组开头插入
		newMessages := []interface{}{systemMsg}
		for _, msg := range messages.Array() {
			newMessages = append(newMessages, msg.Value())
		}
		body, _ = sjson.SetBytes(body, "messages", newMessages)
	}

	return body
}

// removeWebSearchFromBody 从 Chat Completions body 中移除 web_search 工具
func removeWebSearchFromBody(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}

	var newTools []interface{}
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("type").String() != "web_search" {
			newTools = append(newTools, tool.Value())
		}
		return true
	})

	body, _ = sjson.SetBytes(body, "tools", newTools)
	return body
}