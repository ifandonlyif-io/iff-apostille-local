package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// messagesSink emits Anthropic-style SSE. Text deltas are forwarded as they
// pass validation; tool calls are held until the whole stream has validated.
type messagesSink struct {
	out      sseWriter
	id       string
	model    string
	textOpen bool
}

type evt map[string]any

// frame encodes one SSE event. The encoder's trailing newline ends the data
// line; the second newline ends the frame.
func frame(event evt) string {
	return fmt.Sprintf("event: %s\ndata: %s\n", event["type"], encodeNoEscape(event))
}

func (s *messagesSink) start() bool {
	return s.out.send(frame(evt{"type": "message_start", "message": evt{"id": "msg_" + s.id, "type": "message", "role": "assistant", "model": s.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": messagesUsage{}}}))
}

func (s *messagesSink) chunk(_, text string) bool {
	if text == "" {
		return true
	}
	var b strings.Builder
	if !s.textOpen {
		s.textOpen = true
		b.WriteString(frame(evt{"type": "content_block_start", "index": 0, "content_block": evt{"type": "text", "text": ""}}))
	}
	b.WriteString(frame(evt{"type": "content_block_delta", "index": 0, "delta": evt{"type": "text_delta", "text": text}}))
	return s.out.send(b.String())
}

func (s *messagesSink) usage(string, json.RawMessage) bool { return true }

// ready validates the final metadata before commit writes buffered output.
// commit can still fail to write or flush; only success permits a receipt.
func (s *messagesSink) ready(reason string, usage json.RawMessage) bool {
	_, ok := parseMessagesUsage(usage)
	_, ok2 := messagesStopReason(reason)
	return ok && ok2
}

// commit sends the whole buffered tail, including message_delta and
// message_stop, in one write and one flush. A failure anywhere in it leaves a
// failed receipt; there is no point at which only part of it counted as sent.
func (s *messagesSink) commit(reason string, calls []ToolCall, usage json.RawMessage) bool {
	u, _ := parseMessagesUsage(usage)
	stop, _ := messagesStopReason(reason)
	var b strings.Builder
	index := 0
	if s.textOpen {
		b.WriteString(frame(evt{"type": "content_block_stop", "index": 0}))
		index = 1
	}
	for _, c := range calls {
		b.WriteString(frame(evt{"type": "content_block_start", "index": index, "content_block": evt{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": evt{}}}))
		b.WriteString(frame(evt{"type": "content_block_delta", "index": index, "delta": evt{"type": "input_json_delta", "partial_json": c.Function.Arguments}}))
		b.WriteString(frame(evt{"type": "content_block_stop", "index": index}))
		index++
	}
	b.WriteString(frame(evt{"type": "message_delta", "delta": evt{"stop_reason": stop, "stop_sequence": nil}, "usage": u}))
	b.WriteString(frame(evt{"type": "message_stop"}))
	return s.out.send(b.String())
}

func (s *messagesSink) fail() {
	s.out.send(frame(evt{"type": "error", "error": evt{"type": "api_error", "message": "invalid_runtime_stream"}}))
}
