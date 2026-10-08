package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const namedToolChoice = `{"type":"function","function":{"name":"lookup_demo"}}`

func namedToolInput(stream bool) string {
	return strings.Replace(toolInput(stream), `"tool_choice":"required"`, `"tool_choice":`+namedToolChoice, 1)
}

func TestVLLMNamedCompletionAdapterIsExplicitAndValidated(t *testing.T) {
	valid := toolResponse("lookup_demo", `{"code":"TEST-001"}`, "stop")
	// These fields are unrelated to adaptation and must survive it without
	// conversion through float64 or dropping vendor/usage information.
	valid = `{"vendor_counter":9007199254740993,"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"completion_tokens_details":{"reasoning_tokens":0}},` + strings.TrimPrefix(valid, "{")
	for _, tc := range []struct {
		name, profile, input, body string
		ok                         bool
	}{
		{"named_stop", "vllm-chat-v1", namedToolInput(false), valid, true},
		{"strict_default", "", namedToolInput(false), valid, false},
		{"unknown_profile", "other-runtime", namedToolInput(false), valid, false},
		{"required_stop", "vllm-chat-v1", toolInput(false), valid, false},
		{"auto_stop", "vllm-chat-v1", strings.Replace(toolInput(false), `"tool_choice":"required"`, `"tool_choice":"auto"`, 1), valid, false},
		{"already_standard", "vllm-chat-v1", namedToolInput(false), strings.Replace(valid, `"finish_reason":"stop"`, `"finish_reason":"tool_calls"`, 1), true},
		{"wrong_name", "vllm-chat-v1", namedToolInput(false), strings.Replace(valid, "lookup_demo", "unexpected", 1), false},
		{"wrong_argument_type", "vllm-chat-v1", namedToolInput(false), toolResponse("lookup_demo", `{"code":42}`, "stop"), false},
		{"incomplete_arguments", "vllm-chat-v1", namedToolInput(false), toolResponse("lookup_demo", `{"code":`, "stop"), false},
		{"length", "vllm-chat-v1", namedToolInput(false), strings.Replace(valid, `"finish_reason":"stop"`, `"finish_reason":"length"`, 1), false},
		{"plain_text", "vllm-chat-v1", namedToolInput(false), completion("synthetic"), false},
		{"reason_alias", "vllm-chat-v1", namedToolInput(false), strings.Replace(valid, `"finish_reason":"stop"`, `"finish_reason":"length","Finish_Reason":"stop"`, 1), false},
		{"call_alias", "vllm-chat-v1", namedToolInput(false), strings.Replace(valid, `"name":"lookup_demo"`, `"name":"unexpected","Name":"lookup_demo"`, 1), false},
		{"duplicate_reason", "vllm-chat-v1", namedToolInput(false), strings.Replace(valid, `"finish_reason":"stop"`, `"finish_reason":"length","finish_reason":"stop"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }, true)
			g.c.Models[0].ToolCallParser = "hermes"
			g.c.Models[0].RuntimeProfile = tc.profile
			w := request(g, "POST", "/v1/chat/completions", tokenA, tc.input, true)
			if (w.Code == 200) != tc.ok || (!tc.ok && w.Code != 502) {
				t.Fatalf("unexpected status: %d", w.Code)
			}
			rec, err := store.Get("a", w.Header().Get("X-Apostille-Run-ID"))
			if err != nil || (rec.Status == "ready") != tc.ok {
				t.Fatalf("incorrect receipt: %s, %v", rec.Status, err)
			}
			if tc.ok {
				if !strings.Contains(w.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(string(rec.Manifest), `"finish_reason":"tool_calls"`) {
					t.Fatal("wire and receipt must use the normalized reason")
				}
				if !strings.Contains(w.Body.String(), `"vendor_counter":9007199254740993`) || !strings.Contains(w.Body.String(), `"completion_tokens_details":{"reasoning_tokens":0}`) {
					t.Fatal("normalization changed unrelated metadata")
				}
			}
		})
	}
}

func TestVLLMNamedStreamAdapterRequiresCompleteCallsAndDone(t *testing.T) {
	first := streamEvent(callDelta(0, "call_1", "lookup_demo", `{"co`), nil)
	second := streamEvent(callDelta(0, "", "", `de":"TEST-001"}`), nil)
	terminal := streamEvent(map[string]any{}, "stop")
	terminal = strings.Replace(terminal, `{"choices":`, `{"vendor_counter":9007199254740993,"choices":`, 1)
	done := "data: [DONE]\n\n"
	valid := first + second + terminal
	for _, tc := range []struct {
		name, profile, input, events string
		ok                           bool
	}{
		{"named_stop", "vllm-chat-v1", namedToolInput(true), valid + done, true},
		{"named_stop_usage", "vllm-chat-v1", strings.TrimSuffix(namedToolInput(true), "}") + `,"stream_options":{"include_usage":true}}`, valid + usageEvent + done, true},
		{"strict_default", "", namedToolInput(true), valid + done, false},
		{"required_stop", "vllm-chat-v1", toolInput(true), valid + done, false},
		{"auto_stop", "vllm-chat-v1", strings.Replace(toolInput(true), `"tool_choice":"required"`, `"tool_choice":"auto"`, 1), valid + done, false},
		{"missing_done", "vllm-chat-v1", namedToolInput(true), valid, false},
		{"truncated_arguments", "vllm-chat-v1", namedToolInput(true), first + terminal + done, false},
		{"wrong_argument_type", "vllm-chat-v1", namedToolInput(true), streamEvent(callDelta(0, "call_1", "lookup_demo", `{"code":42}`), nil) + terminal + done, false},
		{"wrong_name", "vllm-chat-v1", namedToolInput(true), strings.Replace(valid, "lookup_demo", "unexpected", 1) + done, false},
		{"length", "vllm-chat-v1", namedToolInput(true), first + second + streamEvent(map[string]any{}, "length") + done, false},
		{"multiple_calls", "vllm-chat-v1", namedToolInput(true), first + second + streamEvent(callDelta(1, "call_2", "lookup_demo", `{"code":"x"}`), nil) + terminal + done, false},
		{"reason_alias", "vllm-chat-v1", namedToolInput(true), first + second + strings.Replace(terminal, `"finish_reason":"stop"`, `"finish_reason":"length","Finish_Reason":"stop"`, 1) + done, false},
		{"late_delta", "vllm-chat-v1", namedToolInput(true), valid + streamEvent(map[string]any{"content": "late"}, nil) + done, false},
		{"missing_usage", "vllm-chat-v1", strings.TrimSuffix(namedToolInput(true), "}") + `,"stream_options":{"include_usage":true}}`, valid + done, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.events) }, true)
			g.c.Models[0].ToolCallParser = "hermes"
			g.c.Models[0].RuntimeProfile = tc.profile
			w := request(g, "POST", "/v1/chat/completions", tokenA, tc.input, true)
			if w.Code != 200 || strings.Contains(w.Body.String(), "[DONE]") != tc.ok || strings.Contains(w.Body.String(), "invalid_runtime_stream") == tc.ok {
				t.Fatal("incorrect stream terminal state")
			}
			rec, err := store.Get("a", w.Header().Get("X-Apostille-Run-ID"))
			if err != nil || (rec.Status == "ready") != tc.ok {
				t.Fatalf("incorrect stream receipt: %s, %v", rec.Status, err)
			}
			if tc.ok && (!strings.Contains(w.Body.String(), `"vendor_counter":9007199254740993`) || !strings.Contains(w.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(string(rec.Manifest), `"finish_reason":"tool_calls"`)) {
				t.Fatal("stream normalization lost metadata or terminal reason")
			}
			if tc.ok && strings.Contains(tc.input, "include_usage") && !strings.Contains(w.Body.String(), usageEvent) {
				t.Fatal("adapter changed the separate usage event")
			}
		})
	}
}

func TestRuntimeProfileUsesAdmittedModelSnapshot(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			var gateway *Gateway
			g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				// This synthetic handler runs after admission, before the result
				// is processed. The request must retain its original profile.
				gateway.c.Models[0].RuntimeProfile = ""
				if streaming {
					fmt.Fprint(w, streamEvent(callDelta(0, "call_1", "lookup_demo", `{"code":"TEST-001"}`), "stop")+"data: [DONE]\n\n")
				} else {
					fmt.Fprint(w, toolResponse("lookup_demo", `{"code":"TEST-001"}`, "stop"))
				}
			}, false)
			gateway = g
			g.c.Models[0].ToolCallParser = "hermes"
			g.c.Models[0].RuntimeProfile = "vllm-chat-v1"
			w := request(g, "POST", "/v1/chat/completions", tokenA, namedToolInput(streaming), false)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"finish_reason":"tool_calls"`) || strings.Contains(w.Body.String(), "invalid_runtime") {
				t.Fatal("admitted runtime profile changed during generation")
			}
		})
	}
}

func TestVLLMNamedAdapterDoesNotAllowMultipleCalls(t *testing.T) {
	request, _, err := parseRequest([]byte(namedToolInput(false)), interopModel())
	if err != nil {
		t.Fatal(err)
	}
	request.ParallelToolCalls = nil
	var response map[string]any
	json.Unmarshal([]byte(toolResponse("lookup_demo", `{"code":"x"}`, "stop")), &response)
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	second := ToolCall{ID: "call_2", Type: "function", Function: ToolFunctionCall{Name: "lookup_demo", Arguments: `{"code":"y"}`}}
	message["tool_calls"] = append(calls, second)
	raw, _ := json.Marshal(response)
	if _, _, _, ok := checkCompletionForRuntime(raw, request, "vllm-chat-v1"); ok {
		t.Fatal("named stop with multiple calls was normalized")
	}
}
