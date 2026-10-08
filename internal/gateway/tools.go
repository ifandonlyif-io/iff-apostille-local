package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	schema "github.com/santhosh-tekuri/jsonschema/v6"
)

const maxToolCalls = 16
const maxToolArguments = 64 << 10

var functionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type ToolFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolFunctionCall `json:"function"`
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Strict      *bool          `json:"strict,omitempty"`
}

type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

func messageShape(raw json.RawMessage) bool {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil || o == nil {
		return false
	}
	var role string
	if json.Unmarshal(o["role"], &role) != nil || role == "" {
		return false
	}
	for k, v := range o {
		switch k {
		case "role":
		case "content":
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				if role != "assistant" {
					return false
				}
			} else {
				var content string
				if json.Unmarshal(v, &content) != nil {
					return false
				}
			}
		case "tool_calls":
			if role != "assistant" || !callsShape(v) {
				return false
			}
		case "tool_call_id":
			var id string
			if role != "tool" || json.Unmarshal(v, &id) != nil || !validToolCallID(id) {
				return false
			}
		default:
			return false
		}
	}
	_, contentPresent := o["content"]
	if role != "assistant" && !contentPresent {
		return false
	}
	if role == "assistant" && (!contentPresent || bytes.Equal(bytes.TrimSpace(o["content"]), []byte("null"))) {
		var calls []ToolCall
		if json.Unmarshal(o["tool_calls"], &calls) != nil || len(calls) == 0 {
			return false
		}
	}
	return true
}

func callsShape(raw json.RawMessage) bool {
	var calls []json.RawMessage
	if json.Unmarshal(raw, &calls) != nil || calls == nil || len(calls) > maxToolCalls {
		return false
	}
	for _, call := range calls {
		o, ok := objectShape(call, "id type function", "id type function")
		if !ok {
			return false
		}
		if _, ok := objectShape(o["function"], "name arguments", "name arguments"); !ok {
			return false
		}
	}
	return true
}

func toolsShape(raw json.RawMessage) bool {
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil || len(tools) > 64 {
		return false
	}
	for _, tool := range tools {
		o, ok := objectShape(tool, "type function", "type function")
		if !ok {
			return false
		}
		if _, ok := objectShape(o["function"], "name description parameters strict", "name parameters"); !ok {
			return false
		}
	}
	return true
}

func toolChoiceShape(raw json.RawMessage) bool {
	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		return choice == "none" || choice == "auto" || choice == "required"
	}
	o, ok := objectShape(raw, "type function", "type function")
	if !ok || json.Unmarshal(o["type"], &choice) != nil || choice != "function" {
		return false
	}
	f, ok := objectShape(o["function"], "name", "name")
	return ok && json.Unmarshal(f["name"], &choice) == nil && functionName.MatchString(choice)
}

func validStop(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	valid := func(s string) bool { return len(s) > 0 && len(s) <= 1024 }
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return valid(s)
	}
	var stops []string
	if json.Unmarshal(raw, &stops) != nil || len(stops) < 1 || len(stops) > 4 {
		return false
	}
	for _, stop := range stops {
		if !valid(stop) {
			return false
		}
	}
	return true
}

func validToolCallID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func validCall(call ToolCall) bool {
	if !validToolCallID(call.ID) || call.Type != "function" || !functionName.MatchString(call.Function.Name) || len(call.Function.Arguments) > maxToolArguments {
		return false
	}
	var args map[string]json.RawMessage
	return validJSON([]byte(call.Function.Arguments)) == nil && json.Unmarshal([]byte(call.Function.Arguments), &args) == nil && args != nil
}

// Tools are executed by the caller. The gateway only checks that each result
// answers a previous call exactly once and that a turn is complete before the
// next non-tool message. Historical calls need not use the current tool list.
func validateMessages(messages []Message) bool {
	seen, pending := map[string]bool{}, map[string]bool{}
	for _, m := range messages {
		if len(pending) > 0 && m.Role != "tool" {
			return false
		}
		switch m.Role {
		case "system", "user":
			if m.Content == "" || len(m.ToolCalls) != 0 || m.ToolCallID != "" {
				return false
			}
		case "assistant":
			if (m.Content == "" && len(m.ToolCalls) == 0) || m.ToolCallID != "" || len(m.ToolCalls) > maxToolCalls {
				return false
			}
			for _, call := range m.ToolCalls {
				if !validCall(call) || seen[call.ID] {
					return false
				}
				seen[call.ID], pending[call.ID] = true, true
			}
		case "tool":
			if !pending[m.ToolCallID] || len(m.ToolCalls) != 0 {
				return false
			}
			delete(pending, m.ToolCallID)
		default:
			return false
		}
	}
	return len(pending) == 0
}

func toolChoice(r Request) (mode, name string) {
	if len(r.ToolChoice) == 0 {
		if len(r.Tools) > 0 {
			return "auto", ""
		}
		return "none", ""
	}
	if json.Unmarshal(r.ToolChoice, &mode) == nil {
		return mode, ""
	}
	var named struct {
		Function struct{ Name string }
	}
	_ = json.Unmarshal(r.ToolChoice, &named)
	return "function", named.Function.Name
}

func prepareTools(r *Request, model config.Model) error {
	bad := errors.New("invalid_request")
	usesTools := len(r.Tools) > 0
	for _, m := range r.Messages {
		usesTools = usesTools || len(m.ToolCalls) > 0 || m.Role == "tool"
	}
	if usesTools && model.ToolCallParser == "" {
		return errors.New("tool_calling_unavailable")
	}
	if len(r.Tools) > 64 {
		return bad
	}
	r.toolSchemas = make(map[string]*schema.Schema, len(r.Tools))
	known := make(map[string]bool, len(r.Tools))
	for _, tool := range r.Tools {
		f := tool.Function
		if tool.Type != "function" || !functionName.MatchString(f.Name) || known[f.Name] || len(f.Description) > 4096 || f.Parameters == nil || f.Parameters["type"] != "object" {
			return bad
		}
		known[f.Name] = true
		count := 0
		if checkSchema(f.Parameters, 0, &count) != nil {
			return bad
		}
		c := schema.NewCompiler()
		c.UseLoader(noLoader{})
		c.DefaultDraft(schema.Draft2020)
		if c.AddResource("urn:apostille-local:tool", f.Parameters) != nil {
			return bad
		}
		compiled, err := c.Compile("urn:apostille-local:tool")
		if err != nil {
			return bad
		}
		if f.Strict != nil && *f.Strict {
			r.toolSchemas[f.Name] = compiled
		}
	}
	mode, name := toolChoice(*r)
	if (mode != "none" && len(r.Tools) == 0) || (mode == "function" && !known[name]) || (r.ParallelToolCalls != nil && len(r.Tools) == 0) {
		return bad
	}
	// A tool call is not a structured text answer. Keep the two response
	// contracts unambiguous rather than validating tool arguments as text.
	if r.ResponseFormat != nil && mode != "none" {
		return bad
	}
	return nil
}

// validateCalls verifies a generated final response, including absence of calls
// when required. It never invokes a tool or trusts a name supplied by a model.
func validateCalls(r Request, calls []ToolCall) bool {
	mode, name := toolChoice(r)
	if len(calls) == 0 {
		return mode != "required" && mode != "function"
	}
	if mode == "none" || len(calls) > maxToolCalls || (r.ParallelToolCalls != nil && !*r.ParallelToolCalls && len(calls) > 1) || (mode == "function" && len(calls) != 1) {
		return false
	}
	known := make(map[string]ToolDefinition, len(r.Tools))
	for _, tool := range r.Tools {
		known[tool.Function.Name] = tool.Function
	}
	seen := map[string]bool{}
	for _, m := range r.Messages {
		for _, call := range m.ToolCalls {
			seen[call.ID] = true
		}
	}
	for _, call := range calls {
		f, ok := known[call.Function.Name]
		if !ok || !validCall(call) || seen[call.ID] || (mode == "function" && call.Function.Name != name) {
			return false
		}
		seen[call.ID] = true
		if f.Strict != nil && *f.Strict {
			s := r.toolSchemas[call.Function.Name]
			if s == nil || !validateOutput(s, call.Function.Arguments) {
				return false
			}
		}
	}
	return true
}
