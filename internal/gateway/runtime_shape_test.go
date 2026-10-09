package gateway

import (
	"encoding/json"
	"testing"
)

func TestRuntimeShapeRejectsAmbiguousValidatedFields(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		bodyField := "message"
		if streaming {
			bodyField = "delta"
		}
		baseline := func() map[string]any {
			return map[string]any{
				"id": "synthetic", "object": "chat.completion", "created": 1,
				"model": "qwen3-4b", "choices": []any{map[string]any{
					"index": 0, bodyField: map[string]any{"role": "assistant", "content": "synthetic"}, "finish_reason": "stop", "logprobs": nil,
				}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "completion_tokens_details": map[string]any{"reasoning_tokens": 0}},
			}
		}
		check := func(body map[string]any) bool {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			return runtimeShape(raw, streaming)
		}
		if !check(baseline()) {
			t.Fatal("standard metadata rejected")
		}
		for _, alias := range []string{"Model", "Choices", "Error", "Usage"} {
			body := baseline()
			body[alias] = nil
			if check(body) {
				t.Fatalf("ambiguous top-level %s accepted", alias)
			}
		}
		for _, alias := range []string{"Index", "Message", "Delta", "Finish_Reason"} {
			body := baseline()
			choice := body["choices"].([]any)[0].(map[string]any)
			choice[alias] = nil
			if check(body) {
				t.Fatalf("ambiguous choice %s accepted", alias)
			}
		}
		for _, alias := range []string{"Role", "Content", "Tool_Calls"} {
			body := baseline()
			choice := body["choices"].([]any)[0].(map[string]any)
			choice[bodyField].(map[string]any)[alias] = nil
			if check(body) {
				t.Fatalf("ambiguous message/delta %s accepted", alias)
			}
		}
		for _, alias := range []string{"Prompt_Tokens", "Completion_Tokens", "Total_Tokens"} {
			body := baseline()
			body["usage"].(map[string]any)[alias] = 99
			if check(body) {
				t.Fatalf("ambiguous usage %s accepted", alias)
			}
		}
		for _, mutation := range []string{"missing-model", "missing-choices", "null-choices", "object-choices", "missing-index", "null-index", "missing-message", "null-message"} {
			body := baseline()
			choice := body["choices"].([]any)[0].(map[string]any)
			switch mutation {
			case "missing-model":
				delete(body, "model")
			case "missing-choices":
				delete(body, "choices")
			case "null-choices":
				body["choices"] = nil
			case "object-choices":
				body["choices"] = map[string]any{}
			case "missing-index":
				delete(choice, "index")
			case "null-index":
				choice["index"] = nil
			case "missing-message":
				delete(choice, bodyField)
			case "null-message":
				choice[bodyField] = nil
			}
			if check(body) {
				t.Fatalf("malformed runtime envelope %s accepted", mutation)
			}
		}
	}
}

func TestRuntimeShapeAcceptsUsageAndNullableDeltas(t *testing.T) {
	for _, raw := range []string{
		`{"model":"qwen3-4b","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`{"model":"qwen3-4b","choices":[{"index":0,"delta":{"content":null,"tool_calls":null},"finish_reason":null}],"usage":null}`,
		`{"model":"qwen3-4b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	} {
		if !runtimeShape([]byte(raw), true) {
			t.Fatal("standard stream shape rejected")
		}
	}
}

func TestRuntimeToolAliasesCannotBypassDeclaredFunction(t *testing.T) {
	body := interopRequest()
	body["tools"] = []any{interopTool()}
	req, err := parseInterop(t, body)
	if err != nil {
		t.Fatal(err)
	}
	// A case-sensitive client would see the unlisted exact-lowercase call;
	// encoding/json would overwrite it with the allowed later alias.
	raw := `{"model":"qwen3-4b","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"unlisted","arguments":"{}"}}],"Tool_Calls":[{"id":"call_1","type":"function","function":{"name":"lookup_sample","arguments":"{\"sample\":\"synthetic\"}"}}]},"finish_reason":"tool_calls"}]}`
	if runtimeShape([]byte(raw), false) {
		t.Fatal("ambiguous tool call envelope accepted")
	}
	if _, _, ok := checkCompletion([]byte(raw), req); ok {
		t.Fatal("tool allowlist was bypassed by a case alias")
	}
}
