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
		tool := len(request.Tools) != 0 && string(request.ToolChoice) != `"none"`
		for _, message := range request.Messages {
			if message.Role == "tool" {
				tool = false
			}
		}
		finish := "stop"
		if tool {
			finish = "tool_calls"
			// vLLM's named function path can use stop even though a tool call
			// completed. Exercise the explicit runtime profile adapter rather
			// than teaching clients to accept this backend-specific detail.
			if strings.HasPrefix(string(request.ToolChoice), "{") {
				finish = "stop"
			}
		}
		usage := map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		encodeEvent := func(delta any, reason any) {
			b, _ := json.Marshal(map[string]any{"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "created": 1, "model": "qwen3-4b", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(map[string]any{"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "created": 1, "model": "qwen3-4b", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}}})
			// Exercise a valid multiline SSE JSON event through the real SDK.
			if tool {
				encodeEvent(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_demo", "type": "function", "function": map[string]string{"name": "lookup_demo", "arguments": `{"code":`}}}}, nil)
				encodeEvent(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": `"TEST-001"}`}}}}, nil)
			} else {
				fmt.Fprintf(w, "data: %s\n\n", strings.Replace(string(b), `,"model":`, ",\ndata: \"model\":", 1))
			}
			w.(http.Flusher).Flush()
			if request.Messages[0].Content == "cancel" {
				<-r.Context().Done()
				return
			}
			encodeEvent(map[string]any{}, finish)
			if request.StreamOptions != nil && request.StreamOptions.IncludeUsage {
				b, _ := json.Marshal(map[string]any{"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "created": 1, "model": "qwen3-4b", "choices": []any{}, "usage": usage})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		message := map[string]any{"role": "assistant", "content": content}
		if tool {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "call_demo", "type": "function", "function": map[string]string{"name": "lookup_demo", "arguments": `{"code":"TEST-001"}`}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-synthetic", "object": "chat.completion", "created": 1, "model": "qwen3-4b", "usage": usage, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
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
	model.ToolCallParser = "hermes"
	model.RuntimeProfile = "vllm-chat-v1"
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
	interop := exec.Command(python, "openai_live.py", server.URL, ca, paths[0], paths[1])
	if output, err := interop.CombinedOutput(); err != nil {
		t.Fatalf("official OpenAI SDK integration: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	frameworkPython := os.Getenv("APOSTILLE_FRAMEWORK_PYTHON")
	if frameworkPython == "" {
		frameworkPython = python
	}
	framework := exec.Command(frameworkPython, "langchain_live.py", server.URL, ca, paths[0], paths[1])
	if output, err := framework.CombinedOutput(); err != nil {
		t.Fatalf("LangChain client integration: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	for _, vendor := range []string{"amd", "nvidia"} {
		t.Run("acceptance_cli_"+vendor, func(t *testing.T) {
			// Both operator declarations exercise the same hardware-independent
			// API contract. The mock cannot observe or qualify a real GPU.
			check := exec.Command(python, "../tools/check_gateway.py",
				"--base-url", server.URL, "--token-file", paths[0], "--ca-file", ca,
				"--model", model.ID, "--vendor", vendor, "--require-tools")
			check.Env = append(os.Environ(), "PYTHONPATH=../sdk/python/src")
			var stderr bytes.Buffer
			check.Stderr = &stderr
			output, err := check.Output()
			if err != nil {
				t.Fatalf("acceptance CLI failed: %v", err)
			}
			if stderr.Len() != 0 {
				t.Fatal("acceptance CLI emitted diagnostics")
			}
			for _, marker := range []string{"SYNTHETIC_PRIVATE", "lookup_demo", "TEST-001",
				"synthetic-project-token", "Reply with a short synthetic greeting",
				"Return a JSON object with answer equal to 42"} {
				if bytes.Contains(output, []byte(marker)) {
					t.Fatal("acceptance report contains test content or credentials")
				}
			}
			var report struct {
				Schema             string            `json:"schema"`
				Result             string            `json:"result"`
				RequestedVendor    string            `json:"requested_vendor"`
				HardwareAcceptance string            `json:"hardware_acceptance"`
				GPUIdentity        string            `json:"gpu_identity"`
				Checks             map[string]string `json:"checks"`
			}
			if err := json.Unmarshal(output, &report); err != nil {
				t.Fatal("acceptance report is not a JSON object")
			}
			if report.Schema != "apostille-local-api-check/1" || report.Result != "passed" ||
				report.RequestedVendor != vendor || report.HardwareAcceptance != "unverified" ||
				report.GPUIdentity != "not_observed" {
				t.Fatal("acceptance report violated the API-only result contract")
			}
			names := []string{"discovery", "text", "stream_usage", "json_schema",
				"named_tool", "required_tool", "tool_roundtrip", "named_tool_stream"}
			if len(report.Checks) != len(names) {
				t.Fatal("acceptance report omitted or added checks")
			}
			for _, name := range names {
				if report.Checks[name] != "passed" {
					t.Fatalf("acceptance check %s did not pass", name)
				}
			}
		})
	}
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
		for _, marker := range []string{"SYNTHETIC_PRIVATE_INPUT", "SYNTHETIC_PRIVATE_OUTPUT", "SYNTHETIC_PRIVATE_TOOL_RESULT", "lookup_demo", "TEST-001", "synthetic-project-token"} {
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
