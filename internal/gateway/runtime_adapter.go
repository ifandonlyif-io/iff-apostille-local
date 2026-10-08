package gateway

import "encoding/json"

// normalizeRuntimeFinish is deliberately scoped to an administrator-selected
// wire profile, pinned with the model's asset manifest. vLLM 0.15.0 reports stop
// for a completed named function call; the public contract uses tool_calls.
// This does not turn truncated calls or other runtimes' terminal states into
// success. Call IDs, the selected name, cardinality, JSON and strict schemas
// must all pass the same validation as an unadapted tool_calls response.
func normalizeRuntimeFinish(profile string, request Request, calls []ToolCall, reason string) (string, bool) {
	if profile == "vllm-chat-v1" && reason == "stop" && len(calls) == 1 {
		mode, _ := toolChoice(request)
		if mode == "function" && validateCalls(request, calls) {
			return "tool_calls", true
		}
	}
	return reason, validFinish(request, calls, reason)
}

// rewriteRuntimeFinish is called only after complete shape and semantic
// validation. RawMessage preserves vendor metadata, number precision, usage
// details and tool arguments while changing only the single choice's reason.
func rewriteRuntimeFinish(data []byte, reason string) ([]byte, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return nil, false
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(envelope["choices"], &choices) != nil || len(choices) != 1 || choices[0] == nil {
		return nil, false
	}
	encodedReason, err := json.Marshal(reason)
	if err != nil {
		return nil, false
	}
	choices[0]["finish_reason"] = encodedReason
	envelope["choices"], err = json.Marshal(choices)
	if err != nil {
		return nil, false
	}
	data, err = json.Marshal(envelope)
	return data, err == nil
}
