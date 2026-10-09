package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	schema "github.com/santhosh-tekuri/jsonschema/v6"
)

const anthropicVersion = "2023-06-01"

var (
	errMsgField   = errors.New("unsupported_or_invalid_field")
	errMsgInvalid = errors.New("invalid_request")
)

var messagesDialect = dialect{
	parse:  parseMessagesRequest,
	fail:   messagesFail,
	render: renderMessage,
	sink: func(w http.ResponseWriter, id string, request Request) streamSink {
		return &messagesSink{out: newSSEWriter(w), id: id, model: request.Model}
	},
}

func messagesErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 413:
		return "request_too_large"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	}
	return "invalid_request_error"
}

func messagesFail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(encodeNoEscape(map[string]any{"type": "error", "error": map[string]string{"type": messagesErrorType(status), "message": code}}))
}

// encodeNoEscape keeps model text and tool input byte-faithful: the default
// encoder would rewrite <, > and & inside customer-visible values.
func encodeNoEscape(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return b.Bytes()
}

func (g *Gateway) messages(w http.ResponseWriter, r *http.Request) {
	p, ok := g.authMessages(r)
	if !ok {
		messagesFail(w, 401, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		messagesFail(w, 404, "not_found")
		return
	}
	if v := r.Header.Values("Anthropic-Version"); len(v) != 1 || v[0] != anthropicVersion {
		messagesFail(w, 400, "anthropic_version_unsupported")
		return
	}
	if len(r.Header.Values("Anthropic-Beta")) > 0 {
		messagesFail(w, 400, "beta_not_supported")
		return
	}
	g.infer(w, r, p, messagesDialect)
}

// ---- request translation ----

func parseMessagesRequest(raw []byte, m config.Model) (Request, *schema.Schema, error) {
	if validJSON(raw) != nil {
		return Request{}, nil, errors.New("invalid_json")
	}
	translated, err := translateMessages(raw)
	if err != nil {
		return Request{}, nil, err
	}
	body, err := json.Marshal(translated)
	if err != nil {
		return Request{}, nil, errMsgInvalid
	}
	return parseRequest(body, m)
}

func decodeNumberMap(raw json.RawMessage) (map[string]any, bool) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil || v == nil {
		return nil, false
	}
	return v, true
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	return s, json.Unmarshal(raw, &s) == nil
}

// textBlock accepts exactly {"type":"text","text":...}.
func textBlock(raw json.RawMessage, allowEmpty bool) (string, bool) {
	o, ok := objectShape(raw, "type text", "type text")
	if !ok {
		return "", false
	}
	typ, ok1 := rawString(o["type"])
	text, ok2 := rawString(o["text"])
	return text, ok1 && ok2 && typ == "text" && (text != "" || allowEmpty)
}

func translateMessages(raw []byte) (Request, error) {
	var out Request
	o, ok := objectShape(raw, "model max_tokens messages system stream temperature top_p tools tool_choice output_config", "model max_tokens messages")
	if !ok {
		return out, errMsgField
	}
	var ok2 bool
	if out.Model, ok2 = rawString(o["model"]); !ok2 {
		return out, errMsgField
	}
	if json.Unmarshal(o["max_tokens"], &out.MaxTokens) != nil || out.MaxTokens < 1 {
		return out, errMsgField
	}
	if v, present := o["stream"]; present {
		if json.Unmarshal(v, &out.Stream) != nil {
			return out, errMsgField
		}
		if out.Stream {
			out.StreamOptions = &StreamOptions{IncludeUsage: true}
		}
	}
	if v, present := o["temperature"]; present {
		var t float64
		if json.Unmarshal(v, &t) != nil {
			return out, errMsgField
		}
		if t < 0 || t > 1 {
			return out, errMsgInvalid
		}
		out.Temperature = &t
	}
	if v, present := o["top_p"]; present {
		var t float64
		if json.Unmarshal(v, &t) != nil {
			return out, errMsgField
		}
		if t <= 0 || t > 1 {
			return out, errMsgInvalid
		}
		out.TopP = &t
	}
	if v, present := o["system"]; present {
		text, err := translateSystem(v)
		if err != nil {
			return out, err
		}
		out.Messages = append(out.Messages, Message{Role: "system", Content: text})
	}
	var in []json.RawMessage
	if json.Unmarshal(o["messages"], &in) != nil || in == nil {
		return out, errMsgField
	}
	if len(in) == 0 || len(in) > 128 {
		return out, errMsgInvalid
	}
	lastAssistant := false
	for _, m := range in {
		msgs, assistant, err := translateMessage(m)
		if err != nil {
			return out, err
		}
		out.Messages = append(out.Messages, msgs...)
		lastAssistant = assistant
	}
	if lastAssistant {
		return out, errors.New("assistant_prefill_unsupported")
	}
	if v, present := o["tools"]; present {
		tools, err := translateTools(v)
		if err != nil {
			return out, err
		}
		out.Tools = tools
	}
	if v, present := o["tool_choice"]; present {
		choice, noParallel, err := translateToolChoice(v)
		if err != nil {
			return out, err
		}
		out.ToolChoice = choice
		if noParallel {
			f := false
			out.ParallelToolCalls = &f
		}
	}
	if v, present := o["output_config"]; present {
		f, err := translateOutputConfig(v)
		if err != nil {
			return out, err
		}
		out.ResponseFormat = f
	}
	return out, nil
}

func translateSystem(raw json.RawMessage) (string, error) {
	if s, ok := rawString(raw); ok {
		if s == "" {
			return "", errMsgInvalid
		}
		return s, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) != 1 {
		return "", errMsgField
	}
	if text, ok := textBlock(blocks[0], false); ok {
		return text, nil
	}
	return "", errMsgField
}

// translateMessage returns the internal messages for one Messages-API turn.
func translateMessage(raw json.RawMessage) ([]Message, bool, error) {
	o, ok := objectShape(raw, "role content", "role content")
	if !ok {
		return nil, false, errMsgField
	}
	role, _ := rawString(o["role"])
	if role != "user" && role != "assistant" {
		return nil, false, errMsgField
	}
	if s, isString := rawString(o["content"]); isString {
		if s == "" {
			return nil, false, errMsgInvalid
		}
		return []Message{{Role: role, Content: s}}, role == "assistant", nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(o["content"], &blocks) != nil || blocks == nil {
		return nil, false, errMsgField
	}
	if len(blocks) == 0 || len(blocks) > 128 {
		return nil, false, errMsgInvalid
	}
	if role == "user" {
		msgs, err := translateUserBlocks(blocks)
		return msgs, false, err
	}
	m, err := translateAssistantBlocks(blocks)
	return []Message{m}, true, err
}

func blockType(raw json.RawMessage) string {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil {
		return ""
	}
	t, _ := rawString(o["type"])
	return t
}

func translateUserBlocks(blocks []json.RawMessage) ([]Message, error) {
	var msgs []Message
	text, haveText := "", false
	for _, b := range blocks {
		switch blockType(b) {
		case "text":
			t, ok := textBlock(b, false)
			if !ok {
				return nil, errMsgField
			}
			if haveText {
				return nil, errMsgInvalid
			}
			text, haveText = t, true
		case "tool_result":
			if haveText {
				return nil, errMsgInvalid
			}
			m, err := translateToolResult(b)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m)
		default:
			return nil, errMsgField
		}
	}
	if haveText {
		msgs = append(msgs, Message{Role: "user", Content: text})
	}
	return msgs, nil
}

func translateToolResult(raw json.RawMessage) (Message, error) {
	o, ok := objectShape(raw, "type tool_use_id content is_error", "type tool_use_id")
	if !ok {
		return Message{}, errMsgField
	}
	id, ok := rawString(o["tool_use_id"])
	if !ok {
		return Message{}, errMsgField
	}
	if v, present := o["is_error"]; present {
		var isErr bool
		if json.Unmarshal(v, &isErr) != nil {
			return Message{}, errMsgField
		}
		if isErr {
			return Message{}, errors.New("tool_result_error_unsupported")
		}
	}
	content := ""
	if v, present := o["content"]; present {
		if s, isString := rawString(v); isString {
			content = s
		} else {
			var blocks []json.RawMessage
			if json.Unmarshal(v, &blocks) != nil || len(blocks) != 1 {
				return Message{}, errMsgField
			}
			if content, ok = textBlock(blocks[0], true); !ok {
				return Message{}, errMsgField
			}
		}
	}
	return Message{Role: "tool", ToolCallID: id, Content: content}, nil
}

func translateAssistantBlocks(blocks []json.RawMessage) (Message, error) {
	m := Message{Role: "assistant"}
	haveText := false
	for _, b := range blocks {
		switch blockType(b) {
		case "text":
			t, ok := textBlock(b, false)
			if !ok {
				return m, errMsgField
			}
			if haveText || len(m.ToolCalls) > 0 {
				return m, errMsgInvalid
			}
			m.Content, haveText = t, true
		case "tool_use":
			o, ok := objectShape(b, "type id name input", "type id name input")
			if !ok {
				return m, errMsgField
			}
			id, ok1 := rawString(o["id"])
			name, ok2 := rawString(o["name"])
			var args bytes.Buffer
			if !ok1 || !ok2 || !isJSONObject(o["input"]) || json.Compact(&args, o["input"]) != nil {
				return m, errMsgField
			}
			m.ToolCalls = append(m.ToolCalls, ToolCall{ID: id, Type: "function", Function: ToolFunctionCall{Name: name, Arguments: args.String()}})
		default:
			return m, errMsgField
		}
	}
	return m, nil
}

func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

func translateTools(raw json.RawMessage) ([]Tool, error) {
	var in []json.RawMessage
	if json.Unmarshal(raw, &in) != nil || in == nil {
		return nil, errMsgField
	}
	if len(in) == 0 || len(in) > 64 {
		return nil, errMsgInvalid
	}
	tools := make([]Tool, 0, len(in))
	for _, t := range in {
		o, ok := objectShape(t, "name description input_schema strict type", "name input_schema")
		if !ok {
			return nil, errMsgField
		}
		var f ToolDefinition
		if f.Name, ok = rawString(o["name"]); !ok {
			return nil, errMsgField
		}
		if v, present := o["type"]; present {
			if typ, ok := rawString(v); !ok || typ != "custom" {
				return nil, errMsgField
			}
		}
		if v, present := o["description"]; present {
			if f.Description, ok = rawString(v); !ok {
				return nil, errMsgField
			}
		}
		if v, present := o["strict"]; present {
			var s bool
			if json.Unmarshal(v, &s) != nil {
				return nil, errMsgField
			}
			f.Strict = &s
		}
		if f.Parameters, ok = decodeNumberMap(o["input_schema"]); !ok {
			return nil, errMsgField
		}
		tools = append(tools, Tool{Type: "function", Function: f})
	}
	return tools, nil
}

func translateToolChoice(raw json.RawMessage) (json.RawMessage, bool, error) {
	o, ok := objectShape(raw, "type name disable_parallel_tool_use", "type")
	if !ok {
		return nil, false, errMsgField
	}
	typ, _ := rawString(o["type"])
	_, hasName := o["name"]
	noParallel := false
	if v, present := o["disable_parallel_tool_use"]; present {
		if typ == "none" || json.Unmarshal(v, &noParallel) != nil {
			return nil, false, errMsgField
		}
	}
	if hasName != (typ == "tool") {
		return nil, false, errMsgField
	}
	var choice any
	switch typ {
	case "auto":
		choice = "auto"
	case "any":
		choice = "required"
	case "none":
		choice = "none"
	case "tool":
		name, ok := rawString(o["name"])
		if !ok {
			return nil, false, errMsgField
		}
		choice = map[string]any{"type": "function", "function": map[string]string{"name": name}}
	default:
		return nil, false, errMsgField
	}
	b, err := json.Marshal(choice)
	return b, noParallel, err
}

func translateOutputConfig(raw json.RawMessage) (*Format, error) {
	o, ok := objectShape(raw, "format", "format")
	if !ok {
		return nil, errMsgField
	}
	format, ok := objectShape(o["format"], "type schema", "type schema")
	if !ok {
		return nil, errMsgField
	}
	if typ, ok := rawString(format["type"]); !ok || typ != "json_schema" {
		return nil, errMsgField
	}
	schemaMap, ok := decodeNumberMap(format["schema"])
	if !ok {
		return nil, errMsgField
	}
	f := &Format{Type: "json_schema"}
	f.JSONSchema.Name, f.JSONSchema.Strict, f.JSONSchema.Schema = "response", true, schemaMap
	return f, nil
}

// ---- response translation ----

func messagesStopReason(reason string) (string, bool) {
	switch reason {
	case "stop":
		return "end_turn", true
	case "length":
		return "max_tokens", true
	case "tool_calls":
		return "tool_use", true
	}
	return "", false
}

type messagesUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
}

func parseMessagesUsage(raw json.RawMessage) (messagesUsage, bool) {
	var u struct {
		Prompt     int64 `json:"prompt_tokens"`
		Completion int64 `json:"completion_tokens"`
	}
	if !validUsage(raw, true) || json.Unmarshal(raw, &u) != nil {
		return messagesUsage{}, false
	}
	return messagesUsage{u.Prompt, u.Completion}, true
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type toolUseContent struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func renderMessage(id string, request Request, res completionResult) ([]byte, bool) {
	usage, ok := parseMessagesUsage(res.usage)
	stop, ok2 := messagesStopReason(res.reason)
	if !ok || !ok2 {
		return nil, false
	}
	content := []any{}
	if res.content != "" {
		content = append(content, textContent{"text", res.content})
	}
	for _, c := range res.calls {
		content = append(content, toolUseContent{"tool_use", c.ID, c.Function.Name, json.RawMessage(c.Function.Arguments)})
	}
	return encodeNoEscape(struct {
		ID           string        `json:"id"`
		Type         string        `json:"type"`
		Role         string        `json:"role"`
		Model        string        `json:"model"`
		Content      []any         `json:"content"`
		StopReason   string        `json:"stop_reason"`
		StopSequence *string       `json:"stop_sequence"`
		Usage        messagesUsage `json:"usage"`
	}{"msg_" + id, "message", "assistant", request.Model, content, stop, nil, usage}), true
}
