package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const integrationTool = `{"type":"function","function":{"name":"lookup_demo","strict":true,"parameters":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}}}`

func toolInput(stream bool) string {
	return strings.TrimSuffix(input(stream), "}") + `,"tools":[` + integrationTool + `],"tool_choice":"required","parallel_tool_calls":false}`
}

func toolResponse(name, arguments, finish string) string {
	b, _ := json.Marshal(map[string]any{"model": "qwen3-4b", "choices": []any{map[string]any{
		"index": 0, "message": map[string]any{"role": "assistant", "content": nil,
			"tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]string{"name": name, "arguments": arguments}}}},
		"finish_reason": finish}}})
	return string(b)
}

func streamEvent(delta any, finish any) string {
	b, _ := json.Marshal(map[string]any{"model": "qwen3-4b", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	return "data: " + string(b) + "\n\n"
}

func callDelta(index int, id, name, arguments string) map[string]any {
	d := map[string]any{"index": index, "function": map[string]string{"name": name, "arguments": arguments}}
	if id != "" {
		d["id"], d["type"] = id, "function"
	}
	return map[string]any{"tool_calls": []any{d}}
}

const usageEvent = "data: {\"model\":\"qwen3-4b\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"

func TestCapabilitiesAreScopedAndDoNotExposeDeployment(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {}, true)
	if w := request(g, "GET", "/local/v1/capabilities", "", "", false); w.Code != 401 {
		t.Fatal("anonymous capabilities", w.Code)
	}
	for _, enabled := range []bool{false, true} {
		if enabled {
			g.c.Models[0].ToolCallParser = "hermes"
			g.c.Models[0].RuntimeProfile = "vllm-chat-v1"
		}
		w := request(g, "GET", "/local/v1/capabilities", tokenA, "", false)
		var got struct {
			Models []struct {
				ID       string `json:"id"`
				Max      int    `json:"max_concurrent_requests"`
				Features struct {
					Tools bool `json:"tool_calling"`
				} `json:"features"`
			} `json:"models"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Models) != 1 || got.Models[0].Max != 1 || got.Models[0].Features.Tools != enabled {
			t.Fatal("wrong capabilities", w.Code, w.Body)
		}
		for _, private := range []string{"qwen3-8b", "runtime_url", "api_key", "/synthetic", "project_id"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatal("private deployment data exposed")
			}
		}
	}
	for _, key := range []string{tokenB, tokenA} {
		if key == tokenA {
			g.Drain()
		}
		w := request(g, "GET", "/local/v1/capabilities", key, "", false)
		if !strings.Contains(w.Body.String(), `"models":[]`) {
			t.Fatal("inactive or unauthorized model advertised", w.Body)
		}
	}
}

func TestGeneratedToolCallsAreValidatedBeforeSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", toolResponse("lookup_demo", `{"code":"`+outputMarker+`"}`, "tool_calls"), true},
		{"unknown_function", toolResponse("unexpected", `{"code":"x"}`, "tool_calls"), false},
		{"function_name_alias", strings.Replace(toolResponse("lookup_demo", `{"code":"x"}`, "tool_calls"), `"name":"lookup_demo"`, `"name":"unexpected","Name":"lookup_demo"`, 1), false},
		{"legacy_function", strings.Replace(toolResponse("lookup_demo", `{"code":"x"}`, "tool_calls"), `"role":"assistant"`, `"role":"assistant","function_call":{"name":"unexpected","arguments":"{}"}`, 1), false},
		{"wrong_argument_type", toolResponse("lookup_demo", `{"code":42}`, "tool_calls"), false},
		{"non_json_arguments", toolResponse("lookup_demo", `not-json`, "tool_calls"), false},
		{"truncated", toolResponse("lookup_demo", `{"code":"x"}`, "length"), false},
		{"missing_calls", completion("x"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, store, dir := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }, true)
			g.c.Models[0].ToolCallParser = "hermes"
			g.c.Models[0].RuntimeProfile = "vllm-chat-v1"
			w := request(g, "POST", "/v1/chat/completions", tokenA, toolInput(false), true)
			if (w.Code == 200) != tc.valid {
				t.Fatal(w.Code, w.Body)
			}
			rec, err := store.Get("a", w.Header().Get("X-Apostille-Run-ID"))
			if err != nil || (rec.Status == "ready") != tc.valid {
				t.Fatal("incorrect receipt", err, rec.Status)
			}
			if tc.valid && !strings.Contains(string(rec.Manifest), `"finish_reason":"tool_calls"`) {
				t.Fatal("lost generation finish reason")
			}
			err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if strings.Contains(string(data), outputMarker) || strings.Contains(string(data), "lookup_demo") {
					t.Fatal("tool content persisted")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestToolAndUsageStreamsFailClosed(t *testing.T) {
	first := streamEvent(callDelta(0, "call_1", "lookup_demo", `{"co`), nil)
	second := streamEvent(callDelta(0, "", "", `de":"TEST-001"}`), nil)
	finish := streamEvent(map[string]any{}, "tool_calls")
	valid := first + second + finish
	for _, tc := range []struct {
		name, events string
		usage, ok    bool
	}{
		{"tool_fragments", valid + "data: [DONE]\n\n", false, true},
		{"tool_and_usage", valid + usageEvent + "data: [DONE]\n\n", true, true},
		{"missing_usage", valid + "data: [DONE]\n\n", true, false},
		{"unrequested_usage", valid + usageEvent + "data: [DONE]\n\n", false, false},
		{"duplicate_usage", valid + usageEvent + usageEvent + "data: [DONE]\n\n", true, false},
		{"early_usage", usageEvent + valid + "data: [DONE]\n\n", true, false},
		{"invalid_usage", valid + strings.Replace(usageEvent, `"total_tokens":3`, `"total_tokens":-1`, 1) + "data: [DONE]\n\n", true, false},
		{"missing_done", valid, false, false},
		{"truncated_arguments", first + finish + "data: [DONE]\n\n", false, false},
		{"wrong_name", strings.Replace(valid, "lookup_demo", "unexpected", 1) + "data: [DONE]\n\n", false, false},
		{"function_name_alias", strings.Replace(valid, `"name":"lookup_demo"`, `"name":"unexpected","Name":"lookup_demo"`, 1) + "data: [DONE]\n\n", false, false},
		{"index_gap", streamEvent(callDelta(1, "call_1", "lookup_demo", `{}`), nil) + "data: [DONE]\n\n", false, false},
		{"parallel_forbidden", first + streamEvent(callDelta(1, "call_2", "lookup_demo", `{}`), nil) + "data: [DONE]\n\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.events) }, true)
			g.c.Models[0].ToolCallParser = "hermes"
			g.c.Models[0].RuntimeProfile = "vllm-chat-v1"
			body := toolInput(true)
			if tc.usage {
				body = strings.TrimSuffix(body, "}") + `,"stream_options":{"include_usage":true}}`
			}
			w := request(g, "POST", "/v1/chat/completions", tokenA, body, true)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "[DONE]") != tc.ok || strings.Contains(w.Body.String(), "invalid_runtime_stream") == tc.ok {
				t.Fatal("invalid terminal state", w.Body)
			}
			rec, err := store.Get("a", w.Header().Get("X-Apostille-Run-ID"))
			if err != nil || (rec.Status == "ready") != tc.ok {
				t.Fatal("incorrect stream receipt", err, rec.Status)
			}
		})
	}
}
