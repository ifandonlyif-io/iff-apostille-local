package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func assertReceiptStatus(t *testing.T, g *Gateway, runID, want string) {
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

func TestToolStreamWriteFailuresFailReceipt(t *testing.T) {
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
			assertReceiptStatus(t, g, w.Header().Get("X-Apostille-Run-ID"), "failed")
		})
	}
}

func TestToolStreamFlushedReceiptsAreReady(t *testing.T) {
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
			assertReceiptStatus(t, g, w.Header().Get("X-Apostille-Run-ID"), "ready")
		})
	}
}

// recordingWriter keeps each Write call separately and can delay the first one
// that carries a marker, to order the signed completion time against output.
type recordingWriter struct {
	*httptest.ResponseRecorder
	writes    []string
	slowOn    string
	slowStart time.Time
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.slowOn != "" && w.slowStart.IsZero() && strings.Contains(string(b), w.slowOn) {
		w.slowStart = time.Now().UTC()
		time.Sleep(50 * time.Millisecond)
	}
	w.writes = append(w.writes, string(b))
	return w.ResponseRecorder.Write(b)
}

func recordedCompletion(t *testing.T, messagesAPI bool, w http.ResponseWriter) *Gateway {
	t.Helper()
	g := toolGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, completionUsage(outputMarker, "stop"))
	}, true)
	path, body := "/v1/chat/completions", `{"model":"qwen3-4b","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	if messagesAPI {
		path, body = "/v1/messages", mbody("")
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

func signedCompletion(t *testing.T, g *Gateway, runID string) time.Time {
	t.Helper()
	rec, err := g.store.Get("a", runID)
	var m struct {
		CompletedAt string `json:"completed_at"`
	}
	if err != nil || rec.Status != "ready" || json.Unmarshal(rec.Manifest, &m) != nil {
		t.Fatalf("receipt not ready: %v %q", err, rec.Status)
	}
	completed, err := time.Parse(time.RFC3339Nano, m.CompletedAt)
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

// The buffered tail of a Messages stream is one write and one flush, so the
// stop reason can never be sent while the frames around it are not.
func TestMessagesStreamTailIsOneWrite(t *testing.T) {
	w := &recordingWriter{ResponseRecorder: httptest.NewRecorder()}
	g := recordedToolStream(t, true, w)
	assertReceiptStatus(t, g, w.Header().Get("X-Apostille-Run-ID"), "ready")
	if len(w.writes) != 2 || !strings.HasPrefix(w.writes[0], "event: message_start\n") {
		t.Fatalf("writes = %d, want message_start and one tail", len(w.writes))
	}
	for _, event := range []string{"content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(w.writes[1], "event: "+event+"\n") {
			t.Fatalf("tail write lacks %s", event)
		}
	}
}

// completed_at describes the validated generation, not how long the client
// took to accept the output that follows it.
func TestSignedCompletionPrecedesOutputWrite(t *testing.T) {
	for _, tc := range []struct {
		name        string
		messagesAPI bool
		stream      bool
		slowOn      string
	}{
		{"messages_stream", true, true, "event: message_stop\n"},
		{"chat_stream", false, true, "data: [DONE]"},
		{"messages", true, false, `"stop_reason"`},
		{"chat", false, false, `"choices"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &recordingWriter{ResponseRecorder: httptest.NewRecorder(), slowOn: tc.slowOn}
			var g *Gateway
			if tc.stream {
				g = recordedToolStream(t, tc.messagesAPI, w)
			} else {
				g = recordedCompletion(t, tc.messagesAPI, w)
			}
			completed := signedCompletion(t, g, w.Header().Get("X-Apostille-Run-ID"))
			if w.slowStart.IsZero() || completed.After(w.slowStart) {
				t.Fatalf("completed_at %s is after the output write began at %s", completed, w.slowStart)
			}
		})
	}
}

// Non-streaming responses follow the stream rule: no success receipt unless
// the body was written and flushed.
func TestCompletionWriteFailuresFailReceipt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		messagesAPI bool
		writeMarker string
		flushMarker string
		want        string
	}{
		{name: "messages_write", messagesAPI: true, writeMarker: `"stop_reason"`, want: "failed"},
		{name: "messages_flush", messagesAPI: true, flushMarker: `"stop_reason"`, want: "failed"},
		{name: "chat_write", writeMarker: `"choices"`, want: "failed"},
		{name: "chat_flush", flushMarker: `"choices"`, want: "failed"},
		{name: "messages_ok", messagesAPI: true, want: "ready"},
		{name: "chat_ok", want: "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &deliveryWriter{ResponseRecorder: httptest.NewRecorder(), writeFailureMarker: tc.writeMarker, flushFailureMarker: tc.flushMarker}
			g := recordedCompletion(t, tc.messagesAPI, w)
			if (tc.want == "failed") != w.failed {
				t.Fatalf("synthetic failure exercised = %v", w.failed)
			}
			assertReceiptStatus(t, g, w.Header().Get("X-Apostille-Run-ID"), tc.want)
		})
	}
}
