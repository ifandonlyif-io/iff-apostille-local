package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hold an admitted request before JSON decoding, without a running model.
type heldRequestBody struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	reader  io.Reader
}

func (b *heldRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.reader.Read(p)
}
func (b *heldRequestBody) Close() error { return nil }

type observedRequestBody struct{ reads atomic.Int32 }

func (b *observedRequestBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	return 0, io.EOF
}
func (b *observedRequestBody) Close() error { return nil }

func TestAdmissionBoundsParsingBeforeRuntimeWork(t *testing.T) {
	for _, tc := range []struct {
		name, secondToken, code string
		capacity                int
	}{
		{"project", tokenA, "project_capacity_exceeded", 2},
		{"global", tokenB, "capacity_exceeded", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, completion(outputMarker))
			}, false)
			g.slots = make(chan struct{}, tc.capacity)
			body := &heldRequestBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader("{}")}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(body.release) }) })
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r.Header.Set("Authorization", "Bearer "+tokenA)
			r.Header.Set("Content-Type", "application/json")
			r.Body = body
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				g.ServeHTTP(w, r)
				done <- w
			}()
			select {
			case <-body.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("first request never reached body reading")
			}
			probe := &observedRequestBody{}
			second := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			second.Header.Set("Authorization", "Bearer "+tc.secondToken)
			second.Header.Set("Content-Type", "application/json")
			second.Body = probe
			w := httptest.NewRecorder()
			g.ServeHTTP(w, second)
			if w.Code != 429 || !strings.Contains(w.Body.String(), tc.code) || probe.reads.Load() != 0 {
				t.Fatalf("capacity rejected too late: status=%d reads=%d response=%s", w.Code, probe.reads.Load(), w.Body)
			}
			if calls.Load() != 0 {
				t.Fatal("runtime called before request validation")
			}
			release.Do(func() { close(body.release) })
			select {
			case first := <-done:
				if first.Code != 400 {
					t.Fatalf("invalid admitted body: %d", first.Code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("invalid request did not finish")
			}
			if next := request(g, "POST", "/v1/chat/completions", tokenA, input(false), false); next.Code != 200 {
				t.Fatalf("capacity leaked after parse/rejection: %d", next.Code)
			}
		})
	}
}

func TestAdmissionReleasedAfterValidationErrors(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, completion(outputMarker)) }, false)
	for _, body := range []string{
		"{",
		strings.Repeat("x", (1<<20)+1),
		strings.Replace(input(false), "qwen3-4b", "forbidden-model", 1),
		strings.TrimSuffix(input(false), "}") + `,"response_format":{"type":"json_schema","json_schema":{"name":"bad","strict":true,"schema":{"type":"invalid"}}}}`,
	} {
		if w := request(g, "POST", "/v1/chat/completions", tokenA, body, false); w.Code < 400 || w.Code >= 500 {
			t.Fatalf("expected request rejection: %d", w.Code)
		}
		if w := request(g, "POST", "/v1/chat/completions", tokenA, input(false), false); w.Code != 200 {
			t.Fatalf("capacity leaked after validation failure: %d", w.Code)
		}
	}
}
