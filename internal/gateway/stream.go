package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	schema "github.com/santhosh-tekuri/jsonschema/v6"
)

// streamSink receives only events that already passed runtime validation. The
// loop below owns every check; sinks only encode the public protocol.
type streamSink interface {
	start() bool
	chunk(data, text string) bool
	usage(data string, usage json.RawMessage) bool
	ready(reason string, usage json.RawMessage) bool
	commit(reason string, calls []ToolCall, usage json.RawMessage) bool
	fail()
}

func (g *Gateway) stream(w http.ResponseWriter, r *http.Request, body io.Reader, request Request, s *schema.Schema, finish func(string), runtimeProfile string, sink streamSink) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	if _, ok := w.(http.Flusher); !ok || !sink.start() {
		return
	}
	streamError := sink.fail
	scanner := bufio.NewScanner(io.LimitReader(body, maxResponse+1))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	var event []string
	var content strings.Builder
	var calls []ToolCall
	wantUsage := request.StreamOptions != nil && request.StreamOptions.IncludeUsage
	gotUsage := false
	reason := ""
	var usage json.RawMessage
	total := 0
	done := false
	consume := func() bool {
		if len(event) == 0 {
			return true
		}
		data := strings.Join(event, "\n")
		event = nil
		if data == "[DONE]" {
			// The sink's own final checks run before finish(): a receipt must never
			// complete for a stream the public protocol then reports as failed.
			if reason == "" || !validFinish(request, calls, reason) || (wantUsage && !gotUsage) || !validateOutput(s, content.String()) || !sink.ready(reason, usage) || r.Context().Err() != nil {
				streamError()
				return false
			}
			finish(reason)
			done = true
			sink.commit(reason, calls, usage)
			return false
		}
		var chunk struct {
			Model   string          `json:"model"`
			Error   json.RawMessage `json:"error"`
			Usage   json.RawMessage `json:"usage"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role    string      `json:"role"`
					Content string      `json:"content"`
					Tools   []toolDelta `json:"tool_calls"`
				} `json:"delta"`
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if validJSON([]byte(data)) != nil || !runtimeShape([]byte(data), true) || json.Unmarshal([]byte(data), &chunk) != nil || chunk.Error != nil || chunk.Model != request.Model {
			streamError()
			return false
		}
		if len(chunk.Choices) == 0 {
			// OpenAI's optional final usage event follows the finish event and
			// precedes [DONE]. It is not a second completion or an empty delta.
			if !wantUsage || gotUsage || reason == "" || !validUsage(chunk.Usage, true) {
				streamError()
				return false
			}
			gotUsage, usage = true, chunk.Usage
			return sink.usage(data, chunk.Usage)
		}
		if len(chunk.Choices) != 1 || reason != "" || !validUsage(chunk.Usage, false) {
			streamError()
			return false
		}
		c := chunk.Choices[0]
		if c.Index != 0 || (c.Delta.Role != "" && c.Delta.Role != "assistant") || !appendToolDeltas(request, &calls, c.Delta.Tools) {
			streamError()
			return false
		}
		content.WriteString(c.Delta.Content)
		if c.Finish != nil {
			adaptedReason, ok := normalizeRuntimeFinish(runtimeProfile, request, calls, *c.Finish)
			if !ok {
				streamError()
				return false
			}
			if adaptedReason != *c.Finish {
				adapted, ok := rewriteRuntimeFinish([]byte(data), adaptedReason)
				if !ok {
					streamError()
					return false
				}
				data = string(adapted)
			}
			reason = adaptedReason
		}
		// Each event was parsed before it is forwarded; malformed backend error bodies never escape.
		return sink.chunk(data, c.Delta.Content)
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > maxResponse {
			streamError()
			return
		}
		if line == "" {
			if !consume() {
				return
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			event = append(event, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		} else {
			streamError()
			return
		}
	}
	if !done {
		streamError()
	}
}

type toolDelta struct {
	Index    *int             `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolFunctionCall `json:"function"`
}

func (d *toolDelta) UnmarshalJSON(raw []byte) error {
	bad := errors.New("invalid_tool_delta")
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return bad
	}
	for key, value := range fields {
		switch key {
		case "index", "id", "type":
		case "function":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			var function map[string]json.RawMessage
			if json.Unmarshal(value, &function) != nil || function == nil {
				return bad
			}
			for name := range function {
				if name != "name" && name != "arguments" {
					return bad
				}
			}
		default:
			return bad
		}
	}
	type plain toolDelta
	var value plain
	if json.Unmarshal(raw, &value) != nil || value.Index == nil {
		return bad
	}
	*d = toolDelta(value)
	return nil
}

// Calls may be interleaved by index. Accumulate only bounded in-memory state;
// arguments are untrusted until a valid finish and [DONE] have been received.
func appendToolDeltas(request Request, calls *[]ToolCall, deltas []toolDelta) bool {
	if len(deltas) == 0 {
		return true
	}
	mode, _ := toolChoice(request)
	if len(request.Tools) == 0 || mode == "none" {
		return false
	}
	for _, delta := range deltas {
		if delta.Index == nil || *delta.Index < 0 || *delta.Index >= maxToolCalls || *delta.Index > len(*calls) {
			return false
		}
		if *delta.Index == len(*calls) {
			*calls = append(*calls, ToolCall{})
		}
		call := &(*calls)[*delta.Index]
		call.ID += delta.ID
		call.Function.Name += delta.Function.Name
		call.Function.Arguments += delta.Function.Arguments
		if delta.Type != "" {
			if delta.Type != "function" || (call.Type != "" && call.Type != delta.Type) {
				return false
			}
			call.Type = delta.Type
		}
		if len(call.ID) > 128 || len(call.Function.Name) > 64 || len(call.Function.Arguments) > maxToolArguments {
			return false
		}
	}
	return request.ParallelToolCalls == nil || *request.ParallelToolCalls || len(*calls) <= 1
}
