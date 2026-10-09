package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

func interopModel() config.Model {
	m := testConfig("http://runtime:8000").Active()
	m.ToolCallParser = "hermes"
	m.RuntimeProfile = "vllm-chat-v1"
	return m
}

func interopRequest() map[string]any {
	return map[string]any{
		"model":    "qwen3-4b",
		"messages": []any{map[string]any{"role": "user", "content": "Use synthetic data."}},
	}
}

func interopTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "lookup_sample", "description": "Read a synthetic sample.", "strict": true,
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{"sample": map[string]any{"type": "string"}},
				"required": []string{"sample"}, "additionalProperties": false,
			},
		},
	}
}

func parseInterop(t *testing.T, body map[string]any) (Request, error) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := parseRequest(b, interopModel())
	return r, err
}

func TestFrameworkRequestFieldsAndTokenAlias(t *testing.T) {
	body := interopRequest()
	body["n"] = 1
	body["stop"] = []string{"<end>", "Observation:"}
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	body["max_completion_tokens"] = 32
	r, err := parseInterop(t, body)
	if err != nil || r.MaxTokens != 32 || r.MaxCompletionTokens != 0 || r.N != 1 || !r.StreamOptions.IncludeUsage {
		t.Fatalf("framework request rejected or not normalized: %v", err)
	}
	raw, err := json.Marshal(r)
	if err != nil || strings.Contains(string(raw), "max_completion_tokens") || !strings.Contains(string(raw), `"max_tokens":32`) {
		t.Fatal("token alias was not normalized for the runtime")
	}
	for name, mutate := range map[string]func(map[string]any){
		"multiple choices":             func(b map[string]any) { b["n"] = 2 },
		"zero choices":                 func(b map[string]any) { b["n"] = 0 },
		"fractional choices":           func(b map[string]any) { b["n"] = 1.5 },
		"token conflict":               func(b map[string]any) { b["max_tokens"], b["max_completion_tokens"] = 32, 32 },
		"token alias zero":             func(b map[string]any) { b["max_completion_tokens"] = 0 },
		"token alias over model limit": func(b map[string]any) { b["max_completion_tokens"] = 129 },
		"stop empty":                   func(b map[string]any) { b["stop"] = "" },
		"stop empty list":              func(b map[string]any) { b["stop"] = []string{} },
		"stop null member":             func(b map[string]any) { b["stop"] = []any{"end", nil} },
		"stop limit":                   func(b map[string]any) { b["stop"] = []string{"a", "b", "c", "d", "e"} },
		"stop string limit":            func(b map[string]any) { b["stop"] = strings.Repeat("a", 1025) },
		"nonstream usage":              func(b map[string]any) { b["stream_options"] = map[string]any{"include_usage": true} },
		"unknown stream option": func(b map[string]any) {
			b["stream"] = true
			b["stream_options"] = map[string]any{"include_usage": true, "other": true}
		},
		"null stream option":  func(b map[string]any) { b["stream"] = true; b["stream_options"] = map[string]any{"include_usage": nil} },
		"empty stream option": func(b map[string]any) { b["stream"] = true; b["stream_options"] = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			b := interopRequest()
			mutate(b)
			if _, err := parseInterop(t, b); err == nil {
				t.Fatal("invalid framework field accepted")
			}
		})
	}
}

func TestToolHistoryRoundTripAndCallerExecutionBoundary(t *testing.T) {
	call := ToolCall{ID: "call_synthetic_1", Type: "function", Function: ToolFunctionCall{Name: "lookup_sample", Arguments: `{"sample":"synthetic"}`}}
	for _, content := range []string{"omitted", "null", "text"} {
		t.Run(content, func(t *testing.T) {
			b := interopRequest()
			assistant := map[string]any{"role": "assistant", "tool_calls": []ToolCall{call}}
			if content == "null" {
				assistant["content"] = nil
			} else if content == "text" {
				assistant["content"] = "Checking the sample."
			}
			b["messages"] = append(b["messages"].([]any), assistant, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": ""})
			// The current request may use a different set of tools from history.
			r, err := parseInterop(t, b)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct{ Messages []map[string]any }
			if json.Unmarshal(raw, &wire) != nil {
				t.Fatal("invalid runtime payload")
			}
			if content != "text" && wire.Messages[1]["content"] != nil {
				t.Fatal("assistant without text must be forwarded with null content")
			}
			if wire.Messages[2]["content"] != "" || wire.Messages[2]["tool_call_id"] != call.ID {
				t.Fatal("empty tool result or link was lost")
			}
			m := interopModel()
			m.ToolCallParser = ""
			if _, _, err := parseRequest(raw, m); err == nil || err.Error() != "tool_calling_unavailable" {
				t.Fatal("tool history bypassed the configured model capability")
			}
		})
	}
	for name, messages := range map[string][]any{
		"orphan result":               {map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "result"}},
		"unanswered":                  {map[string]any{"role": "assistant", "tool_calls": []ToolCall{call}}},
		"interrupted":                 {map[string]any{"role": "assistant", "tool_calls": []ToolCall{call}}, map[string]any{"role": "user", "content": "next"}},
		"wrong result id":             {map[string]any{"role": "assistant", "tool_calls": []ToolCall{call}}, map[string]any{"role": "tool", "tool_call_id": "other", "content": "result"}},
		"duplicate result":            {map[string]any{"role": "assistant", "tool_calls": []ToolCall{call}}, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "result"}, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "again"}},
		"null result":                 {map[string]any{"role": "assistant", "tool_calls": []ToolCall{call}}, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": nil}},
		"assistant null without call": {map[string]any{"role": "assistant", "content": nil}},
		"user calls":                  {map[string]any{"role": "user", "content": "x", "tool_calls": []ToolCall{call}}},
		"case alias":                  {map[string]any{"role": "assistant", "content": "x", "Tool_Calls": []ToolCall{call}}},
	} {
		t.Run(name, func(t *testing.T) {
			b := interopRequest()
			b["messages"] = messages
			if _, err := parseInterop(t, b); err == nil {
				t.Fatal("invalid tool conversation accepted")
			}
		})
	}
}

func TestToolDefinitionsAreBoundedAndModelGated(t *testing.T) {
	b := interopRequest()
	b["tools"] = []any{interopTool()}
	raw, _ := json.Marshal(b)
	m := interopModel()
	m.ToolCallParser = ""
	if _, _, err := parseRequest(raw, m); err == nil || err.Error() != "tool_calling_unavailable" {
		t.Fatal("tools accepted without parser capability")
	}
	for name, mutate := range map[string]func(map[string]any, map[string]any){
		"duplicate names":              func(b, f map[string]any) { b["tools"] = []any{interopTool(), interopTool()} },
		"too many tools":               func(b, f map[string]any) { b["tools"] = make([]any, 65) },
		"invalid name":                 func(b, f map[string]any) { f["name"] = "remote.execute" },
		"remote schema":                func(b, f map[string]any) { f["parameters"].(map[string]any)["$ref"] = "https://example.com/schema" },
		"nonobject parameters":         func(b, f map[string]any) { f["parameters"] = map[string]any{"type": "string"} },
		"missing parameters":           func(b, f map[string]any) { delete(f, "parameters") },
		"invalid schema":               func(b, f map[string]any) { f["parameters"].(map[string]any)["required"] = "sample" },
		"null strict":                  func(b, f map[string]any) { f["strict"] = nil },
		"unsupported execution config": func(b, f map[string]any) { f["endpoint"] = "https://example.com" },
		"unknown selected function": func(b, f map[string]any) {
			b["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "absent"}}
		},
		"invalid selected type": func(b, f map[string]any) {
			b["tool_choice"] = map[string]any{"type": "custom", "function": map[string]any{"name": "lookup_sample"}}
		},
		"choice extra fields": func(b, f map[string]any) {
			b["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "lookup_sample", "endpoint": "https://example.com"}}
		},
		"choice composite key": func(b, f map[string]any) {
			b["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "lookup_sample"}, "type function": "unsupported"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := interopRequest()
			tool := interopTool()
			b["tools"] = []any{tool}
			mutate(b, tool["function"].(map[string]any))
			if _, err := parseInterop(t, b); err == nil {
				t.Fatal("invalid tool definition or choice accepted")
			}
		})
	}
	for _, choice := range []any{"auto", "required", map[string]any{"type": "function", "function": map[string]any{"name": "lookup_sample"}}} {
		b := interopRequest()
		b["tool_choice"] = choice
		if _, err := parseInterop(t, b); err == nil {
			t.Fatal("tool selection without tools accepted")
		}
	}
}

func TestGeneratedCallsMatchChoiceArgumentsAndStrictSchema(t *testing.T) {
	b := interopRequest()
	b["tools"] = []any{interopTool()}
	r, err := parseInterop(t, b)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "call_1", Type: "function", Function: ToolFunctionCall{Name: "lookup_sample", Arguments: `{"sample":"synthetic"}`}}
	if !validateCalls(r, nil) || !validateCalls(r, []ToolCall{call}) {
		t.Fatal("valid auto response rejected")
	}
	for name, mutate := range map[string]func(*ToolCall){
		"unknown name":         func(c *ToolCall) { c.Function.Name = "unlisted" },
		"missing id":           func(c *ToolCall) { c.ID = "" },
		"whitespace id":        func(c *ToolCall) { c.ID = "call 1" },
		"unknown type":         func(c *ToolCall) { c.Type = "custom" },
		"null arguments":       func(c *ToolCall) { c.Function.Arguments = `null` },
		"array arguments":      func(c *ToolCall) { c.Function.Arguments = `[]` },
		"bad JSON":             func(c *ToolCall) { c.Function.Arguments = `{"sample":` },
		"duplicate keys":       func(c *ToolCall) { c.Function.Arguments = `{"sample":"a","sample":"b"}` },
		"missing schema field": func(c *ToolCall) { c.Function.Arguments = `{}` },
		"wrong schema type":    func(c *ToolCall) { c.Function.Arguments = `{"sample":42}` },
		"extra schema field":   func(c *ToolCall) { c.Function.Arguments = `{"sample":"synthetic","extra":true}` },
		"arguments over limit": func(c *ToolCall) { c.Function.Arguments = `{"sample":"` + strings.Repeat("x", maxToolArguments) + `"}` },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := call
			mutate(&invalid)
			if validateCalls(r, []ToolCall{invalid}) {
				t.Fatal("invalid generated call accepted")
			}
		})
	}
	if validateCalls(r, []ToolCall{call, call}) || validateCalls(r, make([]ToolCall, maxToolCalls+1)) {
		t.Fatal("duplicate or excessive calls accepted")
	}
	withHistory := r
	withHistory.Messages = append(append([]Message(nil), r.Messages...), Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: call.ID, Content: "synthetic result"})
	if validateCalls(withHistory, []ToolCall{call}) {
		t.Fatal("generated call reused an ID from history")
	}
	other := call
	other.ID = "call_2"
	if !validateCalls(r, []ToolCall{call, other}) {
		t.Fatal("valid parallel calls rejected")
	}
	parallel := false
	r.ParallelToolCalls = &parallel
	if validateCalls(r, []ToolCall{call, other}) {
		t.Fatal("parallel=false ignored")
	}
	r.ToolChoice = json.RawMessage(`"required"`)
	if validateCalls(r, nil) || !validateCalls(r, []ToolCall{call}) {
		t.Fatal("required choice ignored")
	}
	r.ToolChoice = json.RawMessage(`"none"`)
	if !validateCalls(r, nil) || validateCalls(r, []ToolCall{call}) {
		t.Fatal("none choice ignored")
	}
	r.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"lookup_sample"}}`)
	if validateCalls(r, nil) || !validateCalls(r, []ToolCall{call}) {
		t.Fatal("named choice ignored")
	}
}

func TestStructuredTextAndToolGenerationHaveDistinctContracts(t *testing.T) {
	b := interopRequest()
	b["tools"] = []any{interopTool()}
	b["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "strict": true, "schema": map[string]any{"type": "object"}}}
	if _, err := parseInterop(t, b); err == nil {
		t.Fatal("simultaneous tool and structured text generation accepted")
	}
	b["tool_choice"] = "none"
	if _, err := parseInterop(t, b); err != nil {
		t.Fatal("structured text with disabled tools rejected")
	}
}
