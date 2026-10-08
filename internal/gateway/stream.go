package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	schema "github.com/santhosh-tekuri/jsonschema/v6"
)

func (g *Gateway) stream(w http.ResponseWriter, r *http.Request, body io.Reader, s *schema.Schema, finish func(string)) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	emit := func(data string) bool {
		_, e := fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(data, "\n", "\ndata: "))
		flusher.Flush()
		return e == nil
	}
	streamError := func() { emit(`{"error":{"code":"invalid_runtime_stream","message":"invalid_runtime_stream"}}`) }
	scanner := bufio.NewScanner(io.LimitReader(body, maxResponse+1))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	var event []string
	var content strings.Builder
	reason := ""
	total := 0
	done := false
	consume := func() bool {
		if len(event) == 0 {
			return true
		}
		data := strings.Join(event, "\n")
		event = nil
		if data == "[DONE]" {
			if reason == "" || !validateOutput(s, content.String()) || r.Context().Err() != nil {
				streamError()
				return false
			}
			finish(reason)
			done = true
			emit("[DONE]")
			return false
		}
		var chunk struct {
			Model   string          `json:"model"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role    string          `json:"role"`
					Content string          `json:"content"`
					Tools   json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if validJSON([]byte(data)) != nil || json.Unmarshal([]byte(data), &chunk) != nil || chunk.Error != nil || chunk.Model != g.c.ActiveModel || len(chunk.Choices) != 1 || reason != "" {
			streamError()
			return false
		}
		c := chunk.Choices[0]
		if c.Index != 0 || (c.Delta.Role != "" && c.Delta.Role != "assistant") || hasTools(c.Delta.Tools) {
			streamError()
			return false
		}
		content.WriteString(c.Delta.Content)
		if c.Finish != nil {
			if *c.Finish != "stop" && *c.Finish != "length" {
				streamError()
				return false
			}
			reason = *c.Finish
		}
		// Each event was parsed before it is forwarded; malformed backend error bodies never escape.
		return emit(data)
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
