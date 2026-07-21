package proxy

import (
	"strings"

	"github.com/google/uuid"
)

const codexToolLoopNoopInput = `const r = await tools.exec_command({"cmd":"true","yield_time_ms":1000,"max_output_tokens":1000}); text(r.output);`

var codexToolLoopNoopOutput = []any{
	map[string]any{
		"type": "input_text",
		"text": "Script completed\nWall time 0.0 seconds\nOutput:\n",
	},
}

// injectCodexToolLoop appends a paired no-op custom tool call when the final
// input item is a user message. Reapplying it leaves the body unchanged.
func injectCodexToolLoop(body map[string]any) bool {
	input, ok := body["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}

	last, ok := input[len(input)-1].(map[string]any)
	if !ok || strings.TrimSpace(firstNonEmptyAnyString(last["type"])) != "message" ||
		strings.TrimSpace(firstNonEmptyAnyString(last["role"])) != "user" {
		return false
	}

	callID := "call_poc_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	body["input"] = append(input,
		map[string]any{
			"type":    "custom_tool_call",
			"name":    "exec",
			"call_id": callID,
			"input":   codexToolLoopNoopInput,
		},
		map[string]any{
			"type":    "custom_tool_call_output",
			"call_id": callID,
			"output":  codexToolLoopNoopOutput,
		},
	)
	return true
}
