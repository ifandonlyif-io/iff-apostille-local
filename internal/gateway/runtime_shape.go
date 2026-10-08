package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Go's JSON decoder matches struct keys case-insensitively; API clients need
// not. Never validate one spelling and forward another meaning to the caller.
// Keep unrelated runtime metadata extensible, but reserve the exact spellings
// of all fields whose values participate in gateway validation.
func runtimeObject(raw json.RawMessage, interpreted, required string) (map[string]json.RawMessage, bool) {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil || o == nil {
		return nil, false
	}
	for key := range o {
		for _, name := range strings.Fields(interpreted) {
			if key != name && strings.EqualFold(key, name) {
				return nil, false
			}
		}
	}
	for _, name := range strings.Fields(required) {
		if _, ok := o[name]; !ok {
			return nil, false
		}
	}
	return o, true
}

func runtimeShape(raw []byte, streaming bool) bool {
	o, ok := runtimeObject(raw, "model choices error usage", "model choices")
	if !ok {
		return false
	}
	var model string
	if json.Unmarshal(o["model"], &model) != nil || model == "" {
		return false
	}
	var choices []json.RawMessage
	if json.Unmarshal(o["choices"], &choices) != nil || choices == nil {
		return false
	}
	for _, rawChoice := range choices {
		messageField := "message"
		if streaming {
			messageField = "delta"
		}
		choice, ok := runtimeObject(rawChoice, "index message delta finish_reason", "index "+messageField)
		if !ok {
			return false
		}
		var index *int
		if json.Unmarshal(choice["index"], &index) != nil || index == nil {
			return false
		}
		message, ok := runtimeObject(choice[messageField], "role content tool_calls function_call", "")
		if !ok {
			return false
		}
		// Only the declared tool_calls protocol is supported. Some runtimes
		// serialize an unused legacy function_call as null, which is harmless.
		if legacy, exists := message["function_call"]; exists && !bytes.Equal(bytes.TrimSpace(legacy), []byte("null")) {
			return false
		}
	}
	if usage, exists := o["usage"]; exists && !bytes.Equal(bytes.TrimSpace(usage), []byte("null")) {
		if _, ok := runtimeObject(usage, "prompt_tokens completion_tokens total_tokens", "prompt_tokens completion_tokens total_tokens"); !ok {
			return false
		}
	}
	return true
}
