package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	schema "github.com/santhosh-tekuri/jsonschema/v6"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type Format struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string         `json:"name"`
		Strict bool           `json:"strict"`
		Schema map[string]any `json:"schema"`
	} `json:"json_schema"`
}
type Request struct {
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	Stream         bool      `json:"stream,omitempty"`
	Temperature    *float64  `json:"temperature,omitempty"`
	TopP           *float64  `json:"top_p,omitempty"`
	MaxTokens      int       `json:"max_tokens,omitempty"`
	ResponseFormat *Format   `json:"response_format,omitempty"`
}

func strict(raw []byte, v any) error {
	if err := validJSON(raw); err != nil {
		return errors.New("invalid_json")
	}
	if !requestShape(raw) {
		return errors.New("unsupported_or_invalid_field")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return errors.New("unsupported_or_invalid_field")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid_json")
	}
	return nil
}

// encoding/json otherwise matches struct fields case-insensitively and treats
// null scalars as their zero value. Keep the public wire contract exact.
func objectShape(raw json.RawMessage, allowed, required string) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	for k, v := range obj {
		if !strings.Contains(" "+allowed+" ", " "+k+" ") || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, false
		}
	}
	for _, k := range strings.Fields(required) {
		if _, ok := obj[k]; !ok {
			return nil, false
		}
	}
	return obj, true
}

func requestShape(raw []byte) bool {
	o, ok := objectShape(raw, "model messages stream temperature top_p max_tokens response_format", "model messages")
	if !ok {
		return false
	}
	if v, present := o["max_tokens"]; present {
		var n int
		if json.Unmarshal(v, &n) != nil || n < 1 {
			return false
		}
	}
	var messages []json.RawMessage
	if json.Unmarshal(o["messages"], &messages) != nil {
		return false
	}
	for _, message := range messages {
		if _, ok := objectShape(message, "role content", "role content"); !ok {
			return false
		}
	}
	if v, present := o["response_format"]; present {
		format, ok := objectShape(v, "type json_schema", "type json_schema")
		if !ok {
			return false
		}
		if _, ok := objectShape(format["json_schema"], "name strict schema", "name strict schema"); !ok {
			return false
		}
	}
	return true
}

type noLoader struct{}

func (noLoader) Load(string) (any, error) { return nil, errors.New("schema_network_forbidden") }
func parseRequest(raw []byte, m config.Model) (Request, *schema.Schema, error) {
	var r Request
	if err := strict(raw, &r); err != nil {
		return r, nil, err
	}
	bad := func() (Request, *schema.Schema, error) { return r, nil, errors.New("invalid_request") }
	if len(r.Messages) == 0 || len(r.Messages) > 128 || r.Model == "" {
		return bad()
	}
	for _, x := range r.Messages {
		if (x.Role != "system" && x.Role != "user" && x.Role != "assistant") || x.Content == "" {
			return bad()
		}
	}
	if r.MaxTokens == 0 {
		r.MaxTokens = m.MaxTokens
	}
	if r.MaxTokens < 1 || r.MaxTokens > m.MaxTokens {
		return bad()
	}
	if r.Temperature != nil && (math.IsNaN(*r.Temperature) || *r.Temperature < 0 || *r.Temperature > 2) {
		return bad()
	}
	if r.TopP != nil && (*r.TopP <= 0 || *r.TopP > 1) {
		return bad()
	}
	if r.ResponseFormat == nil {
		return r, nil, nil
	}
	f := r.ResponseFormat
	if f.Type != "json_schema" || !f.JSONSchema.Strict || !config.ValidID(f.JSONSchema.Name) || f.JSONSchema.Schema == nil {
		return bad()
	}
	count := 0
	if err := checkSchema(f.JSONSchema.Schema, 0, &count); err != nil {
		return bad()
	}
	c := schema.NewCompiler()
	c.UseLoader(noLoader{})
	c.DefaultDraft(schema.Draft2020)
	if err := c.AddResource("urn:apostille-local:response", f.JSONSchema.Schema); err != nil {
		return bad()
	}
	compiled, err := c.Compile("urn:apostille-local:response")
	if err != nil {
		return bad()
	}
	return r, compiled, nil
}

// Deliberately bounded JSON Schema subset: no refs, regex, applicators or remote resources.
func checkSchema(v any, depth int, n *int) error {
	*n++
	if depth > 12 || *n > 256 {
		return errors.New("schema_limit")
	}
	o, ok := v.(map[string]any)
	if !ok {
		return errors.New("schema_object_required")
	}
	for k, x := range o {
		switch k {
		case "type", "required", "enum", "const", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "title", "description":
		case "additionalProperties":
			if _, ok := x.(bool); !ok {
				return errors.New("boolean_required")
			}
		case "properties":
			p, ok := x.(map[string]any)
			if !ok {
				return errors.New("properties_object_required")
			}
			for _, s := range p {
				if err := checkSchema(s, depth+1, n); err != nil {
					return err
				}
			}
		case "items":
			if err := checkSchema(x, depth+1, n); err != nil {
				return err
			}
		default:
			return errors.New("unsupported_schema_keyword")
		}
	}
	return nil
}
func validateOutput(s *schema.Schema, content string) bool {
	if s == nil {
		return true
	}
	d := json.NewDecoder(bytes.NewBufferString(content))
	d.UseNumber()
	var v any
	if validJSON([]byte(content)) != nil || d.Decode(&v) != nil || d.Decode(&struct{}{}) != io.EOF {
		return false
	}
	return s.Validate(v) == nil
}
