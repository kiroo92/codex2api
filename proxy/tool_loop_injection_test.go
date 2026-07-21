package proxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestInjectCodexToolLoopAppendsPairedNoop(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hello"},
		},
	}

	if !injectCodexToolLoop(body) {
		t.Fatal("injectCodexToolLoop() = false, want true")
	}
	items := body["input"].([]any)
	if len(items) != 3 {
		t.Fatalf("input length = %d, want 3", len(items))
	}
	call := items[1].(map[string]any)
	output := items[2].(map[string]any)
	callID, _ := call["call_id"].(string)
	if !strings.HasPrefix(callID, "call_poc_") || len(callID) != len("call_poc_")+12 {
		t.Fatalf("call_id = %q, want call_poc_ plus 12 hex characters", callID)
	}
	if call["type"] != "custom_tool_call" || call["name"] != "exec" || call["input"] != codexToolLoopNoopInput {
		t.Fatalf("unexpected call item: %#v", call)
	}
	if output["type"] != "custom_tool_call_output" || output["call_id"] != callID {
		t.Fatalf("unexpected output item: %#v", output)
	}

	if injectCodexToolLoop(body) {
		t.Fatal("second injection should be skipped")
	}
	if got := len(body["input"].([]any)); got != 3 {
		t.Fatalf("input length after second injection = %d, want 3", got)
	}
}

func TestInjectCodexToolLoopSkipsUnsupportedInput(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "missing input", body: map[string]any{}},
		{name: "empty input", body: map[string]any{"input": []any{}}},
		{name: "string input", body: map[string]any{"input": "hello"}},
		{name: "assistant message", body: map[string]any{"input": []any{map[string]any{"type": "message", "role": "assistant"}}}},
		{name: "tool output", body: map[string]any{"input": []any{map[string]any{"type": "custom_tool_call_output", "call_id": "call_1"}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if injectCodexToolLoop(tt.body) {
				t.Fatal("injectCodexToolLoop() = true, want false")
			}
		})
	}
}

func TestToolLoopInjectionAppliesToResponsesHTTPAndWebSocketOnly(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexToolLoopInjection = true
	ApplyRuntimeSettings(next)

	raw := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hello"}]}`)

	httpBody, httpInput := PrepareResponsesBody(raw)
	if got := gjson.GetBytes(httpBody, "input.#").Int(); got != 3 {
		t.Fatalf("HTTP input length = %d, want 3; body=%s", got, httpBody)
	}
	if got := gjson.Get(httpInput, "#").Int(); got != 3 {
		t.Fatalf("HTTP expanded input length = %d, want 3", got)
	}

	wsBody, wsInput := PrepareResponsesWebSocketBody(raw)
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 3 {
		t.Fatalf("WebSocket input length = %d, want 3; body=%s", got, wsBody)
	}
	if got := gjson.Get(wsInput, "#").Int(); got != 3 {
		t.Fatalf("WebSocket expanded input length = %d, want 3", got)
	}

	compactBody, compactInput := PrepareCompactResponsesBody(raw)
	if got := gjson.GetBytes(compactBody, "input.#").Int(); got != 1 {
		t.Fatalf("compact input length = %d, want 1; body=%s", got, compactBody)
	}
	if got := gjson.Get(compactInput, "#").Int(); got != 1 {
		t.Fatalf("compact expanded input length = %d, want 1", got)
	}
}
