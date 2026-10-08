package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
)

const tokenA = "synthetic-project-a-token-not-a-secret"
const tokenB = "synthetic-project-b-token-not-a-secret"
const promptMarker = "SYNTHETIC_INPUT_MUST_NOT_PERSIST"
const outputMarker = "SYNTHETIC_OUTPUT_MUST_NOT_PERSIST"

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func testConfig(runtime string) config.Config {
	m := config.Model{ID: "qwen3-4b", Revision: strings.Repeat("a", 40), ManifestSHA256: strings.Repeat("b", 64), License: "Apache-2.0", RuntimeImage: "vllm/vllm-openai@sha256:" + strings.Repeat("c", 64), Precision: "float16", MaxContext: 4096, MaxTokens: 128, MaxConcurrent: 2, Path: "/synthetic/model"}
	m2 := m
	m2.ID = "qwen3-8b"
	return config.Config{Version: 1, Listen: "127.0.0.1:8443", RuntimeURL: runtime, ActiveModel: m.ID, Models: []config.Model{m, m2}, Projects: []config.Project{{ID: "a", APIKeySHA256: digest(tokenA), Models: []string{m.ID, m2.ID}, MaxConcurrent: 1}, {ID: "b", APIKeySHA256: digest(tokenB), Models: []string{m2.ID}, MaxConcurrent: 1}}, TimeoutSeconds: 2}
}
func completion(content string) string {
	b, _ := json.Marshal(map[string]any{"id": "synthetic", "object": "chat.completion", "created": 1, "model": "qwen3-4b", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content, "tool_calls": []any{}}, "finish_reason": "stop"}}})
	return string(b)
}
func chunk(content, finish string) string {
	var f any
	if finish != "" {
		f = finish
	}
	b, _ := json.Marshal(map[string]any{"model": "qwen3-4b", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content, "tool_calls": nil}, "finish_reason": f}}})
	return "data: " + string(b) + "\n\n"
}
func setup(t *testing.T, handler http.HandlerFunc, recorded bool) (*Gateway, *evidence.Store, string) {
	t.Helper()
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("project credential forwarded to runtime")
		}
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"qwen3-4b"}]}`)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		handler(w, r)
	}))
	t.Cleanup(runtime.Close)
	var store *evidence.Store
	dir := t.TempDir()
	if recorded {
		key := filepath.Join(dir, "seed")
		if err := os.WriteFile(key, bytes.Repeat([]byte{42}, 32), 0600); err != nil {
			t.Fatal(err)
		}
		var err error
		store, err = evidence.New(filepath.Join(dir, "records"), key, "00000000-0000-4000-8000-000000000001")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
	}
	g, err := New(testConfig(runtime.URL), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g, store, filepath.Join(dir, "records")
}
func request(g *Gateway, method, path, key, body string, record bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	if record {
		r.Header.Set("X-Apostille-Record", "metadata")
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}
func input(stream bool) string {
	return fmt.Sprintf(`{"model":"qwen3-4b","messages":[{"role":"user","content":%q}],"stream":%t,"temperature":0.5,"max_tokens":32}`, promptMarker, stream)
}

func TestAuthorizationCatalogAndStrictRequests(t *testing.T) {
	var calls atomic.Int32
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, completion(outputMarker)) }, false)
	for _, tc := range []struct {
		method, path, key, body string
		status                  int
	}{
		{"GET", "/v1/models", "", "", 401},
		{"GET", "/v1/models", tokenA, "", 200},
		{"GET", "/v1/models", tokenB, "", 200},
		{"POST", "/v1/chat/completions", tokenB, input(false), 403},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), "qwen3-4b", "qwen3-8b", 1), 409},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), `"stream":false`, `"n":1`, 1), 400},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), `"max_tokens":32`, `"max_tokens":0`, 1), 400},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), `"max_tokens":32`, `"max_tokens":null`, 1), 400},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), `"model":`, `"Model":`, 1), 400},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), `"stream":false`, `"Model":"qwen3-8b"`, 1), 400},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), `"stream":false`, `"endpoint":"https://example.com"`, 1), 400},
		{"POST", "/v1/chat/completions", tokenA, strings.Replace(input(false), `"stream":false`, `"model":"qwen3-4b"`, 1), 400},
		{"POST", "/v1/chat/completions", tokenA, `{"model":"qwen3-4b","messages":[{"role":"tool","content":"x"}]}`, 400},
		{"POST", "/v1/chat/completions", tokenA, input(false), 200},
		{"POST", "/admin/activate", tokenA, `{}`, 404},
		{"GET", "/v1/models?secret=synthetic", tokenA, "", 400},
	} {
		t.Run(fmt.Sprintf("%d-%s-%s", tc.status, tc.method, tc.path), func(t *testing.T) {
			w := request(g, tc.method, tc.path, tc.key, tc.body, false)
			if w.Code != tc.status {
				t.Fatalf("status %d expected %d: %s", w.Code, tc.status, w.Body)
			}
			if tc.key == tokenB && tc.path == "/v1/models" && strings.Contains(w.Body.String(), "qwen3") {
				t.Fatal("unauthorized model listed")
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("rejected request reached runtime: %d", calls.Load())
	}
	g.Drain()
	if w := request(g, "POST", "/v1/chat/completions", tokenA, input(false), false); w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestMetadataReceiptAndNoContentPersistence(t *testing.T) {
	g, store, dir := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, completion(outputMarker)) }, true)
	w := request(g, "POST", "/v1/chat/completions", tokenA, input(false), true)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	id := w.Header().Get("X-Apostille-Run-ID")
	rec, err := store.Get("a", id)
	if err != nil || rec.Status != "ready" {
		t.Fatalf("record %v %s", err, rec.Status)
	}
	if w := request(g, "GET", "/local/v1/runs/"+id+"/evidence", tokenB, "", false); w.Code != 404 {
		t.Fatal("cross-project evidence visible")
	}
	if err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		for _, marker := range []string{promptMarker, outputMarker, tokenA, tokenB} {
			if bytes.Contains(b, []byte(marker)) {
				t.Fatal("content persisted")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w = request(g, "POST", "/v1/chat/completions", tokenA, input(false), false)
	if _, err = store.Get("a", w.Header().Get("X-Apostille-Run-ID")); err != evidence.ErrNotFound {
		t.Fatal("default created evidence")
	}
}

func TestSchemaValidatedAndRuntimeFailuresSanitized(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"valid", completion(`{"answer":42}`), 200},
		{"wrong_type", completion(`{"answer":"no"}`), 502},
		{"extra_property", completion(`{"answer":42,"secret":true}`), 502},
		{"invalid_json", completion(`{"answer":42} tail`), 502},
		{"backend_error", `{"error":{"message":"` + promptMarker + `"}}`, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }, true)
			body := strings.TrimSuffix(input(false), "}") + `,"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}}}}`
			w := request(g, "POST", "/v1/chat/completions", tokenA, body, true)
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			rec, e := store.Get("a", w.Header().Get("X-Apostille-Run-ID"))
			if e != nil {
				t.Fatal(e)
			}
			if tc.status != 200 && (rec.Status != "failed" || strings.Contains(w.Body.String(), promptMarker)) {
				t.Fatal("failed request leaked or signed")
			}
		})
	}
	for _, key := range []string{"$ref", "pattern", "allOf", "format"} {
		body := strings.TrimSuffix(input(false), "}") + fmt.Sprintf(`,"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"%s":"https://example.com"}}}}`, key)
		if _, _, err := parseRequest([]byte(body), testConfig("http://runtime:8000").Active()); err == nil {
			t.Fatalf("allowed %s", key)
		}
	}
}

func TestStreamCompletionAndTruncation(t *testing.T) {
	for _, tc := range []struct{ name, body, status string }{
		{"complete", chunk(outputMarker, "") + chunk("", "stop") + "data: [DONE]\n\n", "ready"},
		{"truncated", chunk(outputMarker, "") + chunk("", "stop"), "failed"},
		{"no_finish", chunk(outputMarker, "") + "data: [DONE]\n\n", "failed"},
		{"error", chunk(outputMarker, "") + `data: {"error":{"message":"secret"}}` + "\n\n", "failed"},
		{"bad_reason", chunk(outputMarker, "tool_calls") + "data: [DONE]\n\n", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			}, true)
			w := request(g, "POST", "/v1/chat/completions", tokenA, input(true), true)
			rec, e := store.Get("a", w.Header().Get("X-Apostille-Run-ID"))
			if e != nil || rec.Status != tc.status {
				t.Fatalf("%v %s %s", e, rec.Status, w.Body)
			}
			if tc.status == "failed" && (strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "secret")) {
				t.Fatal("failed stream marked successful or leaked backend error")
			}
		})
	}
}

func TestConcurrencyAndCancellationPropagate(t *testing.T) {
	entered := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chunk(outputMarker, ""))
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	}, true)
	server := httptest.NewServer(g)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(input(true)))
	r.Header.Set("Authorization", "Bearer "+tokenA)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Apostille-Record", "metadata")
	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	if w := request(g, "POST", "/v1/chat/completions", tokenA, input(false), false); w.Code != 429 {
		t.Fatal("concurrent request admitted")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not canceled")
	}
	id := resp.Header.Get("X-Apostille-Run-ID")
	deadline := time.Now().Add(time.Second)
	for {
		rec, e := store.Get("a", id)
		if e == nil && rec.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled status: %v %s", e, rec.Status)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTimeoutRedirectAndRuntimeCrash(t *testing.T) {
	for _, kind := range []string{"timeout", "redirect", "oom"} {
		t.Run(kind, func(t *testing.T) {
			g, store, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "timeout":
					<-r.Context().Done()
				case "redirect":
					http.Redirect(w, r, "http://127.0.0.1:1/private", 302)
				default:
					w.WriteHeader(500)
					fmt.Fprint(w, promptMarker)
				}
			}, true)
			g.c.TimeoutSeconds = 1
			w := request(g, "POST", "/v1/chat/completions", tokenA, input(false), true)
			if w.Code != 502 && w.Code != 504 {
				t.Fatalf("%d", w.Code)
			}
			rec, e := store.Get("a", w.Header().Get("X-Apostille-Run-ID"))
			if e != nil || rec.Status != "failed" {
				t.Fatalf("%v %s", e, rec.Status)
			}
			if strings.Contains(w.Body.String(), promptMarker) {
				t.Fatal("runtime error exposed")
			}
		})
	}
}

func TestNonStreamingGenerationCanExceedTenSeconds(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(11 * time.Second):
			fmt.Fprint(w, completion(outputMarker))
		case <-r.Context().Done():
		}
	}, false)
	g.c.TimeoutSeconds = 15
	w := request(g, "POST", "/v1/chat/completions", tokenA, input(false), false)
	if w.Code != 200 {
		t.Fatalf("configured inference budget was shortened: %d", w.Code)
	}
}

func TestStrictJSON(t *testing.T) {
	bs := string(rune(92))
	for _, s := range []string{`{"x":1,"x":2}`, `{} {}`, `{"x":"` + bs + `ud800` + bs + `ud800"}`, `{"x":"` + bs + `udc00"}`, string([]byte{'"', 0xff, '"'})} {
		if validJSON([]byte(s)) == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	for _, s := range []string{`{"x":1.5}`, `{"x":"` + bs + `ud83d` + bs + `ude00"}`, `{"x":"` + bs + bs + `ud800"}`, `{"x":"�"}`} {
		if e := validJSON([]byte(s)); e != nil {
			t.Fatal("valid JSON rejected", e)
		}
	}
}

func TestSigningFailureDoesNotWithdrawAnswer(t *testing.T) {
	g, store, dir := setup(t, func(w http.ResponseWriter, r *http.Request) {
		os.RemoveAll(dirForFailure.Load().(string))
		fmt.Fprint(w, completion(outputMarker))
	}, true)
	dirForFailure.Store(dir)
	w := request(g, "POST", "/v1/chat/completions", tokenA, input(false), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), outputMarker) {
		t.Fatal("inference withdrawn")
	}
	if rec, err := store.Get("a", w.Header().Get("X-Apostille-Run-ID")); err == nil && rec.Status == "ready" {
		t.Fatal("unavailable storage reported success")
	}
}

var dirForFailure atomic.Value

func FuzzStrictJSON(f *testing.F) {
	for _, s := range []string{`{"value":1}`, `{"x":"hello"}`, `null`, `[true,false]`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if validJSON([]byte(s)) == nil {
			var v any
			d := json.NewDecoder(strings.NewReader(s))
			d.UseNumber()
			if d.Decode(&v) != nil {
				t.Fatal("accepted invalid JSON")
			}
			if _, e := io.ReadAll(d.Buffered()); e != nil {
				t.Fatal(e)
			}
		}
	})
}
