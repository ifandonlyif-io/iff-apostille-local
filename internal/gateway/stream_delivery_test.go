package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// deliveryWriter models failures reported by either Write or the network flush.
// It chooses failures from public SSE bytes, independently of the stream sink.
type deliveryWriter struct {
	*httptest.ResponseRecorder
	writeFailureMarker string
	flushFailureMarker string
	partialWrite       bool
	failed             bool
}

func (w *deliveryWriter) Write(b []byte) (int, error) {
	if w.failed {
		return 0, io.ErrClosedPipe
	}
	if w.writeFailureMarker != "" {
		previous := w.Body.String()
		if at := strings.Index(previous+string(b), w.writeFailureMarker); at >= 0 {
			n := 0
			if w.partialWrite {
				// Accept only the argument prefix, then fail before the frame ends.
				n = at + len(w.writeFailureMarker) - len(previous)
				if n > 0 {
					_, _ = w.ResponseRecorder.Write(b[:n])
				}
			}
			w.failed = true
			return n, io.ErrClosedPipe
		}
	}
	return w.ResponseRecorder.Write(b)
}

func (w *deliveryWriter) Flush() { _ = w.FlushError() }

func (w *deliveryWriter) FlushError() error {
	if w.failed || (w.flushFailureMarker != "" && strings.Contains(w.Body.String(), w.flushFailureMarker)) {
		w.failed = true
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}

func recordedToolStream(t *testing.T, messagesAPI bool, w http.ResponseWriter) *Gateway {
	t.Helper()
	runtime := streamEvent(callDelta(0, "c1", "lookup_demo", `{"code":"x"}`), nil) +
		streamEvent(map[string]any{}, "tool_calls") + usageEvent + "data: [DONE]\n\n"
	g := toolGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, runtime)
	}, true)
	path := "/v1/chat/completions"
	body := `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true},"tools":[` + wantTool + `]}`
	if messagesAPI {
		path = "/v1/messages"
		body = mbody(`,"stream":true,"tools":[` + msgTool + `]`)
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Apostille-Record", "metadata")
	if messagesAPI {
		r.Header.Set("X-Api-Key", tokenA)
		r.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		r.Header.Set("Authorization", "Bearer "+tokenA)
	}
	g.ServeHTTP(w, r)
	return g
}

func assertDeliveredReceipt(t *testing.T, g *Gateway, runID, want string) {
	t.Helper()
	if runID == "" {
		t.Fatal("missing run ID")
	}
	w := request(g, http.MethodGet, "/local/v1/runs/"+runID+"/evidence", tokenA, "", false)
	var receipt struct {
		Status string `json:"receipt_status"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatalf("receipt retrieval failed: HTTP %d", w.Code)
	}
	if receipt.Status != want {
		t.Fatalf("receipt status = %q, want %q", receipt.Status, want)
	}
}

func TestToolStreamDeliveryFailuresFailReceipt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		messagesAPI   bool
		writeMarker   string
		flushMarker   string
		partialWrite  bool
		wantDelivered string
	}{
		{name: "messages_first_tool_block", messagesAPI: true, writeMarker: "event: content_block_start\n"},
		{name: "messages_partial_tool_arguments", messagesAPI: true, writeMarker: `\"code\"`, partialWrite: true, wantDelivered: "event: content_block_delta\n"},
		{name: "messages_stop_reason", messagesAPI: true, writeMarker: "event: message_delta\n"},
		{name: "messages_terminal_write", messagesAPI: true, writeMarker: "event: message_stop\n"},
		{name: "messages_terminal_flush", messagesAPI: true, flushMarker: "event: message_stop\n"},
		{name: "chat_tool_delta", writeMarker: `"tool_calls"`},
		{name: "chat_terminal_write", writeMarker: "data: [DONE]\n\n"},
		{name: "chat_terminal_flush", flushMarker: "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &deliveryWriter{
				ResponseRecorder:   httptest.NewRecorder(),
				writeFailureMarker: tc.writeMarker,
				flushFailureMarker: tc.flushMarker,
				partialWrite:       tc.partialWrite,
			}
			g := recordedToolStream(t, tc.messagesAPI, w)
			if !w.failed {
				t.Fatal("synthetic delivery failure was not exercised")
			}
			if tc.wantDelivered != "" && !strings.Contains(w.Body.String(), tc.wantDelivered) {
				t.Fatal("expected a partially delivered tool argument frame")
			}
			if tc.partialWrite && strings.HasSuffix(w.Body.String(), "\n\n") {
				t.Fatal("argument write unexpectedly delivered a complete frame")
			}
			assertDeliveredReceipt(t, g, w.Header().Get("X-Apostille-Run-ID"), "failed")
		})
	}
}

func TestToolStreamDeliveredReceiptsAreReady(t *testing.T) {
	for _, tc := range []struct {
		name        string
		messagesAPI bool
		terminal    string
	}{
		{name: "messages", messagesAPI: true, terminal: "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{name: "chat", terminal: "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &deliveryWriter{ResponseRecorder: httptest.NewRecorder()}
			g := recordedToolStream(t, tc.messagesAPI, w)
			if w.Code != http.StatusOK || !w.Flushed || !strings.HasSuffix(w.Body.String(), tc.terminal) {
				t.Fatal("successful stream did not deliver its terminal frame")
			}
			assertDeliveredReceipt(t, g, w.Header().Get("X-Apostille-Run-ID"), "ready")
		})
	}
}
