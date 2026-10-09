package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// messagesSink emits Anthropic-style SSE. Text deltas are forwarded as they
// pass validation; tool calls are held until the whole stream has validated.
type messagesSink struct {
	w        http.ResponseWriter
	id       string
	model    string
	textOpen bool
}

func (s *messagesSink) emit(event evt) bool {
	b := encodeNoEscape(event)
	// The encoder's trailing newline ends the data line; the second one ends the frame.
	_, e := fmt.Fprintf(s.w, "event: %s\ndata: %s\n", event["type"], b)
	if e != nil {
		return false
	}
	return http.NewResponseController(s.w).Flush() == nil
}

type evt map[string]any

func (s *messagesSink) start() bool {
	return s.emit(evt{"type": "message_start", "message": evt{"id": "msg_" + s.id, "type": "message", "role": "assistant", "model": s.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": messagesUsage{}}})
}

func (s *messagesSink) chunk(_, text string) bool {
	if text == "" {
		return true
	}
	if !s.textOpen {
		s.textOpen = true
		if !s.emit(evt{"type": "content_block_start", "index": 0, "content_block": evt{"type": "text", "text": ""}}) {
			return false
		}
	}
	return s.emit(evt{"type": "content_block_delta", "index": 0, "delta": evt{"type": "text_delta", "text": text}})
}

func (s *messagesSink) usage(string, json.RawMessage) bool { return true }

// ready validates the final metadata before commit writes buffered output.
// commit can still fail to write or flush; only success permits a receipt.
func (s *messagesSink) ready(reason string, usage json.RawMessage) bool {
	_, ok := parseMessagesUsage(usage)
	_, ok2 := messagesStopReason(reason)
	return ok && ok2
}

func (s *messagesSink) commit(reason string, calls []ToolCall, usage json.RawMessage) bool {
	u, _ := parseMessagesUsage(usage)
	stop, _ := messagesStopReason(reason)
	index := 0
	if s.textOpen {
		if !s.emit(evt{"type": "content_block_stop", "index": 0}) {
			return false
		}
		index = 1
	}
	for _, c := range calls {
		if !s.emit(evt{"type": "content_block_start", "index": index, "content_block": evt{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": evt{}}}) ||
			!s.emit(evt{"type": "content_block_delta", "index": index, "delta": evt{"type": "input_json_delta", "partial_json": c.Function.Arguments}}) ||
			!s.emit(evt{"type": "content_block_stop", "index": index}) {
			return false
		}
		index++
	}
	return s.emit(evt{"type": "message_delta", "delta": evt{"stop_reason": stop, "stop_sequence": nil}, "usage": u}) &&
		s.emit(evt{"type": "message_stop"})
}

func (s *messagesSink) fail() {
	s.emit(evt{"type": "error", "error": evt{"type": "api_error", "message": "invalid_runtime_stream"}})
}
