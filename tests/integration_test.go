//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/gateway"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func TestPythonSDKAgainstTLSGateway(t *testing.T) {
	dir := t.TempDir()
	verifier := filepath.Join(dir, "apostille-local-verify")
	build := exec.Command("go", "build", "-o", verifier, "./cmd/apostille-local-verify")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build offline verifier: %v\n%s", err, output)
	}
	exports := filepath.Join(dir, "downloads")
	if err := os.Mkdir(exports, 0700); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"qwen3-4b"}]}`)
			return
		}
		var request gateway.Request
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		content := "SYNTHETIC_PRIVATE_OUTPUT"
		if request.ResponseFormat != nil {
			content = `{"answer":42}`
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(map[string]any{"model": "qwen3-4b", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}}})
			// Exercise a valid multiline SSE JSON event through the real SDK.
			fmt.Fprintf(w, "data: %s\n\n", strings.Replace(string(b), `,"model":`, ",\ndata: \"model\":", 1))
			w.(http.Flusher).Flush()
			if request.Messages[0].Content == "cancel" {
				<-r.Context().Done()
				return
			}
			fmt.Fprint(w, "data: {\"model\":\"qwen3-4b\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "qwen3-4b", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	}))
	defer backend.Close()
	key := filepath.Join(dir, "seed")
	seed := bytes.Repeat([]byte{17}, 32)
	if err := os.WriteFile(key, seed, 0600); err != nil {
		t.Fatal(err)
	}
	// Provision the receiver pin from the fixture's independently held seed,
	// never from the evidence API response or its embedded public key.
	signer, err := core.NewSigner(base64.RawURLEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	producerPin := signer.KeyID()
	store, err := evidence.New(filepath.Join(dir, "records"), key, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	model := config.Model{ID: "qwen3-4b", Revision: strings.Repeat("a", 40), ManifestSHA256: strings.Repeat("b", 64), RuntimeImage: "vllm/vllm-openai@sha256:" + strings.Repeat("c", 64), License: "Apache-2.0", Precision: "float16", MaxContext: 4096, MaxTokens: 128, MaxConcurrent: 2, Path: "/synthetic"}
	other := model
	other.ID = "qwen3-8b"
	cfg := config.Config{Version: 1, Listen: "127.0.0.1:0", RuntimeURL: backend.URL, Models: []config.Model{model, other}, ActiveModel: model.ID, TimeoutSeconds: 10}
	paths := []string{}
	for _, id := range []string{"a", "b"} {
		token := "synthetic-project-token-not-secret-" + id
		h := sha256.Sum256([]byte(token))
		m := model.ID
		if id == "b" {
			m = other.ID
		}
		cfg.Projects = append(cfg.Projects, config.Project{ID: id, APIKeySHA256: hex.EncodeToString(h[:]), Models: []string{m}, MaxConcurrent: 1})
		p := filepath.Join(dir, "token-"+id)
		if err = os.WriteFile(p, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	g, err := gateway.New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	server := httptest.NewTLSServer(g)
	defer server.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	python := os.Getenv("APOSTILLE_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	cmd := exec.Command(python, "sdk_live.py", server.URL, ca, paths[0], paths[1], verifier, producerPin, exports)
	cmd.Env = append(os.Environ(), "PYTHONPATH=../sdk/python/src")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("SDK integration: %v\n%s", err, output)
	}
	t.Log(string(output))
	scanPrivate := func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		for _, marker := range []string{"SYNTHETIC_PRIVATE_INPUT", "SYNTHETIC_PRIVATE_OUTPUT", "synthetic-project-token"} {
			if bytes.Contains(b, []byte(marker)) {
				return fmt.Errorf("private test marker persisted")
			}
		}
		return nil
	}
	for _, tree := range []string{filepath.Join(dir, "records"), exports} {
		if err = filepath.WalkDir(tree, scanPrivate); err != nil {
			t.Fatal(err)
		}
	}
}
