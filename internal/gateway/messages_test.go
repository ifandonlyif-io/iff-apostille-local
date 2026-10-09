package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

func toolModel() config.Model {
	m := testConfig("http://runtime:8000").Active()
	m.ToolCallParser, m.RuntimeProfile = "hermes", "vllm-chat-v1"
	return m
}

const msgTool = `{"name":"lookup_demo","description":"d","strict":true,"input_schema":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}}`
const wantTool = `{"type":"function","function":{"name":"lookup_demo","description":"d","parameters":{"additionalProperties":false,"properties":{"code":{"type":"string"}},"required":["code"],"type":"object"},"strict":true}}`

func mbody(extra string) string {
	return `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

func TestMessagesTranslation(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"string_content", mbody(""), `{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`},
		{"array_content_system_string", `{"model":"qwen3-4b","max_tokens":32,"system":"sys","temperature":0.5,"top_p":1,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			`{"model":"qwen3-4b","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"temperature":0.5,"top_p":1,"max_tokens":32}`},
		{"system_array", `{"model":"qwen3-4b","max_tokens":32,"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hi"}]}`,
			`{"model":"qwen3-4b","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"max_tokens":32}`},
		{"stream", mbody(`,"stream":true`), `{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":32,"stream_options":{"include_usage":true}}`},
		{"tool_round_trip", `{"model":"qwen3-4b","max_tokens":32,"tools":[` + msgTool + `],"tool_choice":{"type":"none"},"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"lookup_demo","input":{"code": "A<B"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"r1"},{"type":"text","text":"next"}]}]}`,
			`{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"lookup_demo","arguments":"{\"code\":\"A\u003cB\"}"}}]},` +
				`{"role":"tool","content":"r1","tool_call_id":"toolu_1"},{"role":"user","content":"next"}],"max_tokens":32,"tools":[` + wantTool + `],"tool_choice":"none"}`},
		{"tool_only_assistant_and_block_result", `{"model":"qwen3-4b","max_tokens":32,"tools":[` + msgTool + `],"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup_demo","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"x"}]}]}]}`,
			`{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"lookup_demo","arguments":"{}"}}],"content":null},{"role":"tool","content":"x","tool_call_id":"t1"}],"max_tokens":32,"tools":[` + wantTool + `]}`},
		{"choice_any", mbody(`,"tools":[` + msgTool + `],"tool_choice":{"type":"any"}`), `{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"tools":[` + wantTool + `],"tool_choice":"required"}`},
		{"choice_auto_no_parallel", mbody(`,"tools":[` + msgTool + `],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`), `{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"tools":[` + wantTool + `],"tool_choice":"auto","parallel_tool_calls":false}`},
		{"choice_tool", mbody(`,"tools":[` + msgTool + `],"tool_choice":{"type":"tool","name":"lookup_demo","disable_parallel_tool_use":true}`), `{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"tools":[` + wantTool + `],"tool_choice":{"function":{"name":"lookup_demo"},"type":"function"},"parallel_tool_calls":false}`},
		{"output_config", mbody(`,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}}}`),
			`{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"response_format":{"type":"json_schema","json_schema":{"name":"response","strict":true,"schema":{"additionalProperties":false,"properties":{"answer":{"type":"integer"}},"required":["answer"],"type":"object"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, err := parseMessagesRequest([]byte(tc.in), toolModel())
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(req)
			if string(got) != tc.want {
				t.Fatalf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestMessagesRejections(t *testing.T) {
	user := func(c string) string {
		return `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup_demo","input":{}}]},{"role":"user","content":` + c + `}]}`
	}
	asst := func(c string) string {
		return `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":` + c + `},{"role":"user","content":"x"}]}`
	}
	tools := `,"tools":[` + msgTool + `]`
	for _, tc := range []struct{ name, in, code string }{
		{"unknown_key", mbody(`,"extra":1`), "unsupported_or_invalid_field"},
		{"null_value", mbody(`,"system":null`), "unsupported_or_invalid_field"},
		{"case_variant", strings.Replace(mbody(""), `"model"`, `"Model"`, 1), "unsupported_or_invalid_field"},
		{"duplicate_key", mbody(`,"stream":true,"stream":false`), "invalid_json"},
		{"malformed", `{"model":`, "invalid_json"},
		{"image_block", `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`, "unsupported_or_invalid_field"},
		{"thinking_block", asst(`[{"type":"thinking","thinking":"x","signature":"s"}]`), "unsupported_or_invalid_field"},
		{"cache_control", `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`, "unsupported_or_invalid_field"},
		{"citations", `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"x","citations":[]}]}]}`, "unsupported_or_invalid_field"},
		{"stop_sequences", mbody(`,"stop_sequences":["x"]`), "unsupported_or_invalid_field"},
		{"top_k", mbody(`,"top_k":5`), "unsupported_or_invalid_field"},
		{"metadata", mbody(`,"metadata":{"user_id":"u"}`), "unsupported_or_invalid_field"},
		{"is_error_true", user(`[{"type":"tool_result","tool_use_id":"t1","content":"x","is_error":true}]`), "tool_result_error_unsupported"},
		{"text_before_result", user(`[{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"t1","content":"x"}]`), "invalid_request"},
		{"text_after_tool_use", asst(`[{"type":"tool_use","id":"t1","name":"lookup_demo","input":{}},{"type":"text","text":"x"}]`), "invalid_request"},
		{"two_text_blocks", `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`, "invalid_request"},
		{"final_assistant", `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"pre"}]}`, "assistant_prefill_unsupported"},
		{"server_tool_type", mbody(`,"tools":[{"type":"web_search_20250305","name":"web_search"}]`), "unsupported_or_invalid_field"},
		{"eager_input_streaming", mbody(`,"tools":[{"name":"lookup_demo","eager_input_streaming":true,"input_schema":{"type":"object"}}]`), "unsupported_or_invalid_field"},
		{"tool_cache_control", mbody(`,"tools":[{"name":"lookup_demo","cache_control":{"type":"ephemeral"},"input_schema":{"type":"object"}}]`), "unsupported_or_invalid_field"},
		{"output_effort", mbody(`,"output_config":{"effort":"high"}`), "unsupported_or_invalid_field"},
		{"missing_max_tokens", `{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}]}`, "unsupported_or_invalid_field"},
		{"max_tokens_over_limit", strings.Replace(mbody(""), `"max_tokens":32`, `"max_tokens":129`, 1), "invalid_request"},
		{"max_tokens_fraction", strings.Replace(mbody(""), `"max_tokens":32`, `"max_tokens":1.5`, 1), "unsupported_or_invalid_field"},
		{"temperature_high", mbody(`,"temperature":1.5`), "invalid_request"},
		{"empty_messages", `{"model":"qwen3-4b","max_tokens":32,"messages":[]}`, "invalid_request"},
		{"tool_choice_without_tools", mbody(`,"tool_choice":{"type":"any"}`), "invalid_request"},
		{"disable_parallel_on_none", mbody(tools + `,"tool_choice":{"type":"none","disable_parallel_tool_use":true}`), "unsupported_or_invalid_field"},
		{"name_on_auto", mbody(tools + `,"tool_choice":{"type":"auto","name":"lookup_demo"}`), "unsupported_or_invalid_field"},
		{"tool_input_not_object", asst(`[{"type":"tool_use","id":"t1","name":"lookup_demo","input":[]}]`), "unsupported_or_invalid_field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseMessagesRequest([]byte(tc.in), toolModel())
			if err == nil || err.Error() != tc.code {
				t.Fatalf("got %v want %s", err, tc.code)
			}
		})
	}
	// Tool use is rejected like on chat when the model has no parser.
	if _, _, err := parseMessagesRequest([]byte(mbody(`,"tools":[`+msgTool+`]`)), testConfig("http://runtime:8000").Active()); err == nil || err.Error() != "tool_calling_unavailable" {
		t.Fatal(err)
	}
}

func mreq(g *Gateway, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func mh(extra ...string) map[string]string {
	h := map[string]string{"Anthropic-Version": "2023-06-01", "X-Api-Key": tokenA}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] == "" {
			delete(h, extra[i])
		} else {
			h[extra[i]] = extra[i+1]
		}
	}
	return h
}

func errType(t *testing.T, w *httptest.ResponseRecorder, status int, typ, code string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Type != "error" || e.Error.Type != typ || e.Error.Message != code {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("headers", w.Header())
	}
}

func TestMessagesAuthAndHeaders(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, completionUsage("hi", "stop")) }, false)
	ok := mbody("")
	if w := mreq(g, "/v1/messages", mh(), ok); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := mreq(g, "/v1/messages", mh("X-Api-Key", "", "Authorization", "Bearer "+tokenA), ok); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	errType(t, mreq(g, "/v1/messages", mh("Authorization", "Bearer "+tokenA), ok), 401, "authentication_error", "unauthorized")
	errType(t, mreq(g, "/v1/messages", mh("X-Api-Key", ""), ok), 401, "authentication_error", "unauthorized")
	errType(t, mreq(g, "/v1/messages", mh("X-Api-Key", "synthetic-wrong-token-not-a-secret-xx"), ok), 401, "authentication_error", "unauthorized")
	errType(t, mreq(g, "/v1/messages", mh("X-Api-Key", "short"), ok), 401, "authentication_error", "unauthorized")
	errType(t, mreq(g, "/v1/messages", mh("X-Api-Key", tokenA+" x"), ok), 401, "authentication_error", "unauthorized")
	errType(t, mreq(g, "/v1/messages", mh("Anthropic-Version", ""), ok), 400, "invalid_request_error", "anthropic_version_unsupported")
	errType(t, mreq(g, "/v1/messages", mh("Anthropic-Version", "2024-01-01"), ok), 400, "invalid_request_error", "anthropic_version_unsupported")
	errType(t, mreq(g, "/v1/messages", mh("Anthropic-Beta", "files-api-2025-04-14"), ok), 400, "invalid_request_error", "beta_not_supported")
	errType(t, mreq(g, "/v1/messages", mh(), strings.Replace(ok, `"qwen3-4b"`, `"qwen3-8b"`, 1)), 409, "invalid_request_error", "model_inactive")
	errType(t, mreq(g, "/v1/messages", mh("X-Api-Key", tokenB), ok), 403, "permission_error", "model_forbidden")
	errType(t, mreq(g, "/v1/messages", mh(), `{`), 400, "invalid_request_error", "invalid_json")
	errType(t, mreq(g, "/v1/messages", mh("Content-Type", "text/plain"), ok), 415, "invalid_request_error", "json_required")
	if w := mreq(g, "/v1/messages?beta=true", mh(), ok); w.Code != 400 || !strings.Contains(w.Body.String(), "query_not_supported") {
		t.Fatal(w.Code, w.Body)
	}
	// X-Api-Key never authenticates the other endpoints.
	if w := mreq(g, "/v1/chat/completions", mh(), input(false)); w.Code != 401 || !strings.Contains(w.Body.String(), "apostille_local_error") {
		t.Fatal(w.Code, w.Body)
	}
	g.Drain()
	errType(t, mreq(g, "/v1/messages", mh(), ok), 503, "api_error", "draining")
}

func TestMessagesCapabilityAdvertised(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {}, false)
	w := request(g, "GET", "/local/v1/capabilities", tokenA, "", false)
	var c struct {
		Family string              `json:"api_family"`
		APIs   []map[string]string `json:"compatible_apis"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	want := []map[string]string{{"family": "anthropic_messages", "path": "/v1/messages", "anthropic_version": "2023-06-01"}}
	if c.Family != "chat_completions" || !reflect.DeepEqual(c.APIs, want) {
		t.Fatal(w.Body)
	}
}

func completionUsage(content, finish string) string {
	return fmt.Sprintf(`{"model":"qwen3-4b","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":%q}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`, content, finish)
}

func toolCompletion(finish string, usage bool, calls ...string) string {
	u := ""
	if usage {
		u = `,"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}`
	}
	return `{"model":"qwen3-4b","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` + strings.Join(calls, ",") + `]},"finish_reason":"` + finish + `"}]` + u + `}`
}

func rtCall(id, args string) string {
	b, _ := json.Marshal(args)
	return `{"id":"` + id + `","type":"function","function":{"name":"lookup_demo","arguments":` + string(b) + `}}`
}

func toolGateway(t *testing.T, handler http.HandlerFunc, recorded bool) *Gateway {
	g, _, _ := setup(t, handler, recorded)
	g.c.Models[0].ToolCallParser = "hermes"
	g.c.Models[0].RuntimeProfile = "vllm-chat-v1"
	return g
}

func decodeMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err, s)
	}
	return v
}

func TestMessagesNonStreaming(t *testing.T) {
	toolBody := mbody(`,"tools":[` + msgTool + `],"tool_choice":{"type":"tool","name":"lookup_demo"}`)
	for _, tc := range []struct {
		name, runtime, body string
		status              int
		want                string
	}{
		{"text", completionUsage(outputMarker, "stop"), mbody(""), 200,
			`{"id":"msg_RUN","type":"message","role":"assistant","model":"qwen3-4b","content":[{"type":"text","text":"` + outputMarker + `"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`},
		{"length", completionUsage("cut", "length"), mbody(""), 200,
			`{"id":"msg_RUN","type":"message","role":"assistant","model":"qwen3-4b","content":[{"type":"text","text":"cut"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`},
		{"empty_content", completionUsage("", "stop"), mbody(""), 200,
			`{"id":"msg_RUN","type":"message","role":"assistant","model":"qwen3-4b","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`},
		{"tool_use", toolCompletion("tool_calls", true, rtCall("call_1", `{"code": "A<B&C>"}`)), toolBody, 200,
			`{"id":"msg_RUN","type":"message","role":"assistant","model":"qwen3-4b","content":[{"type":"tool_use","id":"call_1","name":"lookup_demo","input":{"code":"A<B&C>"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`},
		{"vllm_named_stop_adapted", toolCompletion("stop", true, rtCall("call_1", `{"code":"x"}`)), toolBody, 200,
			`{"id":"msg_RUN","type":"message","role":"assistant","model":"qwen3-4b","content":[{"type":"tool_use","id":"call_1","name":"lookup_demo","input":{"code":"x"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`},
		{"missing_usage", completion("x"), mbody(""), 502, ""},
		{"invalid_tool_args", toolCompletion("tool_calls", true, rtCall("call_1", `{"code":1}`)), toolBody, 502, ""},
		{"truncated_call", toolCompletion("length", true, rtCall("call_1", `{"code":"x"}`)), toolBody, 502, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := toolGateway(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.runtime) }, true)
			w := mreq(g, "/v1/messages", mh("X-Apostille-Record", "metadata"), tc.body)
			id := w.Header().Get("X-Apostille-Run-ID")
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			rec, err := g.store.Get("a", id)
			if tc.status != 200 {
				errType(t, w, 502, "api_error", "invalid_runtime_response")
				if err != nil || rec.Status != "failed" {
					t.Fatal(err, rec.Status)
				}
				return
			}
			if !reflect.DeepEqual(decodeMap(t, w.Body.String()), decodeMap(t, strings.ReplaceAll(tc.want, "msg_RUN", "msg_"+id))) {
				t.Fatalf("%s", w.Body)
			}
			if strings.Contains(w.Body.String(), `\u003c`) || w.Header().Get("Content-Type") != "application/json" {
				t.Fatal("escaped HTML or wrong type", w.Body)
			}
			if err != nil || rec.Status != "ready" {
				t.Fatal(err, rec.Status)
			}
			if strings.Contains(string(rec.Manifest), `"tool_use"`) || !strings.Contains(string(rec.Manifest), `"finish_reason":"`+map[string]string{"end_turn": "stop", "max_tokens": "length", "tool_use": "tool_calls"}[decodeMap(t, w.Body.String())["stop_reason"].(string)]+`"`) {
				t.Fatalf("receipt finish reason not internal: %s", rec.Manifest)
			}
		})
	}
}

func TestMessagesRuntimeRequestIsInternalChat(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, completionUsage("ok", "stop"))
	}, false)
	// The runtime is called on the chat path only.
	var path string
	g.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		return http.DefaultTransport.RoundTrip(r)
	})
	if w := mreq(g, "/v1/messages", mh(), mbody("")); w.Code != 200 || path != "/v1/chat/completions" {
		t.Fatal(w.Code, path)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sse(t *testing.T, body string) (events []string, data []map[string]any) {
	t.Helper()
	for _, frame := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("bad frame %q", frame)
		}
		events = append(events, strings.TrimPrefix(lines[0], "event: "))
		data = append(data, decodeMap(t, strings.TrimPrefix(lines[1], "data: ")))
	}
	return
}

func TestMessagesStreaming(t *testing.T) {
	toolBody := mbody(`,"stream":true,"tools":[` + msgTool + `]`)
	start := `{"type":"message_start","message":{"id":"msg_RUN","type":"message","role":"assistant","model":"qwen3-4b","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
	textStart := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	delta := func(i int, s string) string {
		return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, i, s)
	}
	stop := func(i int) string { return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) }
	toolStart := func(i int, id string) string {
		return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":"lookup_demo","input":{}}}`, i, id)
	}
	jdelta := func(i int, s string) string {
		return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, i, s)
	}
	end := func(reason string) []string {
		return []string{`{"type":"message_delta","delta":{"stop_reason":"` + reason + `","stop_sequence":null},"usage":{"input_tokens":1,"output_tokens":2}}`, `{"type":"message_stop"}`}
	}
	txt := func(s string) string { return streamEvent(map[string]any{"role": "assistant", "content": s}, nil) }
	fin := func(reason string) string { return streamEvent(map[string]any{}, reason) }
	call := func(i int, id, name, args string) string { return streamEvent(callDelta(i, id, name, args), nil) }
	for _, tc := range []struct {
		name, runtime string
		want          []string
	}{
		{"text", txt("a") + txt("") + txt("b") + fin("stop") + usageEvent + "data: [DONE]\n\n",
			append([]string{start, textStart, delta(0, "a"), delta(0, "b"), stop(0)}, end("end_turn")...)},
		{"length", txt("a") + fin("length") + usageEvent + "data: [DONE]\n\n",
			append([]string{start, textStart, delta(0, "a"), stop(0)}, end("max_tokens")...)},
		{"tool_only", call(0, "c1", "lookup_demo", `{"code"`) + call(0, "", "", `:"x<"}`) + fin("tool_calls") + usageEvent + "data: [DONE]\n\n",
			append([]string{start, toolStart(0, "c1"), jdelta(0, `{"code":"x<"}`), stop(0)}, end("tool_use")...)},
		{"text_and_two_calls_interleaved", txt("hi") + call(0, "c1", "lookup_demo", `{"co`) + call(1, "c2", "lookup_demo", `{"code":`) + call(0, "", "", `de":"a"}`) + txt("!") + call(1, "", "", `"b"}`) + fin("tool_calls") + usageEvent + "data: [DONE]\n\n",
			append([]string{start, textStart, delta(0, "hi"), delta(0, "!"), stop(0), toolStart(1, "c1"), jdelta(1, `{"code":"a"}`), stop(1), toolStart(2, "c2"), jdelta(2, `{"code":"b"}`), stop(2)}, end("tool_use")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := toolGateway(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.runtime)
			}, true)
			w := mreq(g, "/v1/messages", mh("X-Apostille-Record", "metadata"), toolBody)
			id := w.Header().Get("X-Apostille-Run-ID")
			if w.Header().Get("Content-Type") != "text/event-stream" || w.Header().Get("X-Accel-Buffering") != "no" {
				t.Fatal(w.Header())
			}
			events, data := sse(t, w.Body.String())
			var want []map[string]any
			for _, e := range tc.want {
				want = append(want, decodeMap(t, strings.ReplaceAll(e, "msg_RUN", "msg_"+id)))
			}
			if !reflect.DeepEqual(data, want) {
				t.Fatalf("events %v\n%s", events, w.Body)
			}
			for i, e := range events {
				if e != want[i]["type"] {
					t.Fatalf("event name %s != type %v", e, want[i]["type"])
				}
			}
			if rec, err := g.store.Get("a", id); err != nil || rec.Status != "ready" {
				t.Fatal(err, rec.Status)
			}
		})
	}
}

func TestMessagesStreamFailuresNeverCommit(t *testing.T) {
	toolBody := mbody(`,"stream":true,"tools":[` + msgTool + `]`)
	txt := streamEvent(map[string]any{"role": "assistant", "content": "partial"}, nil)
	for _, tc := range []struct{ name, runtime string }{
		{"truncated_no_done", txt + streamEvent(map[string]any{}, "stop") + usageEvent},
		{"no_finish", txt + usageEvent + "data: [DONE]\n\n"},
		{"missing_usage", txt + streamEvent(map[string]any{}, "stop") + "data: [DONE]\n\n"},
		{"backend_error", txt + `data: {"error":{"message":"` + promptMarker + `"}}` + "\n\n"},
		{"truncated_call", streamEvent(callDelta(0, "c1", "lookup_demo", `{"code":"x"}`), nil) + streamEvent(map[string]any{}, "length") + usageEvent + "data: [DONE]\n\n"},
		{"invalid_args", streamEvent(callDelta(0, "c1", "lookup_demo", `{"code":1}`), nil) + streamEvent(map[string]any{}, "tool_calls") + usageEvent + "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := toolGateway(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.runtime)
			}, true)
			w := mreq(g, "/v1/messages", mh("X-Apostille-Record", "metadata"), toolBody)
			events, data := sse(t, w.Body.String())
			last := len(events) - 1
			if events[last] != "error" || !reflect.DeepEqual(data[last], decodeMap(t, `{"type":"error","error":{"type":"api_error","message":"invalid_runtime_stream"}}`)) {
				t.Fatalf("%v", events)
			}
			for _, e := range events {
				if e == "message_delta" || e == "message_stop" {
					t.Fatal("committed failed stream", events)
				}
			}
			if strings.Contains(w.Body.String(), promptMarker) || strings.Contains(w.Body.String(), "tool_use") {
				t.Fatal("leaked backend text or unvalidated call")
			}
			if rec, err := g.store.Get("a", w.Header().Get("X-Apostille-Run-ID")); err != nil || rec.Status != "failed" {
				t.Fatal(err, rec.Status)
			}
		})
	}
}

func FuzzMessagesRequest(f *testing.F) {
	for _, s := range []string{mbody(""), mbody(`,"stream":true,"tools":[` + msgTool + `],"tool_choice":{"type":"any"}`), `{}`, `null`,
		`{"model":"qwen3-4b","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"y"}]}]}`} {
		f.Add(s)
	}
	m := toolModel()
	f.Fuzz(func(t *testing.T, s string) {
		req, _, err := parseMessagesRequest([]byte(s), m)
		if err != nil {
			return
		}
		body, e := json.Marshal(req)
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = parseRequest(body, m); e != nil {
			t.Fatalf("accepted request does not round trip: %v", e)
		}
	})
}

func TestMessagesCancellationFailsReceipt(t *testing.T) {
	entered, canceled := make(chan struct{}, 1), make(chan struct{}, 1)
	g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chunk(outputMarker, ""))
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	}, true)
	server := httptest.NewServer(g)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", strings.NewReader(mbody(`,"stream":true`)))
	for k, v := range mh("X-Apostille-Record", "metadata", "Content-Type", "application/json") {
		r.Header.Set(k, v)
	}
	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not canceled")
	}
	deadline := time.Now().Add(time.Second)
	for {
		rec, e := store.Get("a", resp.Header.Get("X-Apostille-Run-ID"))
		if e == nil && rec.Status == "failed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled status: %v %s", e, rec.Status)
		}
		time.Sleep(time.Millisecond)
	}
}

// The stream loop calls ready before writing buffered output. Validation
// failures must be caught here; commit can still fail on write or flush errors.
func TestMessagesSinkReadyGuardsCommit(t *testing.T) {
	s := &messagesSink{}
	valid := json.RawMessage(`{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}`)
	for _, c := range []struct {
		reason string
		usage  json.RawMessage
		want   bool
	}{
		{"stop", valid, true},
		{"tool_calls", valid, true},
		{"stop", nil, false},
		{"stop", json.RawMessage(`{"prompt_tokens":1,"completion_tokens":2,"total_tokens":4}`), false},
		{"content_filter", valid, false},
	} {
		if got := s.ready(c.reason, c.usage); got != c.want {
			t.Errorf("ready(%q, %s) = %v", c.reason, c.usage, got)
		}
	}
}
