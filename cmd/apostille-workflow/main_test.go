package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflow "github.com/ifandonlyif-io/iff-apostille-local/internal/workflowevidence"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic disk full") }

func TestCLIEndToEndAndSafeFailures(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "receipts")
	if err := os.Mkdir(archive, 0700); err != nil {
		t.Fatal(err)
	}
	id := func() string {
		v, err := core.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	agent := id()
	key := filepath.Join(dir, "private-seed")
	call := func(args ...string) (int, map[string]any, string) {
		t.Helper()
		var out, diagnostic bytes.Buffer
		code := run(args, &out, &diagnostic)
		var value map[string]any
		if out.Len() > 0 && json.Unmarshal(out.Bytes(), &value) != nil {
			t.Fatal("invalid JSON output", out.String())
		}
		return code, value, diagnostic.String()
	}
	code, generated, diagnostic := call("keygen", "--out-key", key, "--agent-id", agent)
	if code != 0 || generated["agent_id"] != agent || diagnostic != "" || len(generated) != 2 {
		t.Fatal(code, generated, diagnostic)
	}
	pin, ok := generated["producer_pin"].(string)
	if !ok || !strings.HasPrefix(pin, "sha256:") {
		t.Fatal(generated)
	}
	e := workflow.Event{Schema: workflow.EventSchema, EvidenceScope: workflow.Scope, ProjectID: id(), JobID: id(), AgentID: agent, EventID: id(), ConfigurationID: id(), ModelID: id(), Sequence: "1", EventType: "model_released", Framework: "generic", FrameworkVersion: "1.0.0"}
	write := func(path string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(dir, "event.json")
	write(input, e)
	policyPath := filepath.Join(dir, "policy.json")
	p := workflow.Policy{Schema: workflow.PolicySchema, ProjectID: e.ProjectID, JobID: e.JobID, Producers: []workflow.Producer{{AgentID: agent, KeyID: pin, EventTypes: []string{"model_released"}}}}
	write(policyPath, p)
	artifact := filepath.Join(dir, "model.synthetic")
	if err := os.WriteFile(artifact, []byte("public synthetic bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(archive, "release.json")
	code, signed, _ := call("sign", "--event", input, "--key-file", key, "--agent-id", agent, "--out", receipt, "--artifact", artifact)
	if code != 0 || signed["status"] != "ready" || signed["event_id"] != e.EventID || signed["producer_pin"] != pin {
		t.Fatal(code, signed)
	}
	for _, withArtifact := range []bool{false, true} {
		args := []string{"verify", "--receipt", receipt, "--policy", policyPath}
		if withArtifact {
			args = append(args, "--artifact", artifact)
		}
		code, v, _ := call(args...)
		binding := "not_checked"
		if withArtifact {
			binding = "matched"
		}
		if code != 0 || v["valid"] != true || v["artifact_binding"] != binding || v["actual_execution"] != "unknown" || v["current_authorization"] != "unknown" {
			t.Fatal(code, v)
		}
	}
	code, set, _ := call("verify-set", "--directory", archive, "--policy", policyPath)
	if code != 0 || set["record_count"] != float64(1) || set["archive_completeness"] != "unknown" {
		t.Fatal(code, set)
	}
	before, _ := os.ReadFile(receipt)
	failures := [][]string{
		{"keygen", "--out-key", key, "--agent-id", agent},
		{"sign", "--event", input, "--key-file", key, "--agent-id", agent, "--out", receipt},
		{"verify", "--receipt", receipt, "--policy", policyPath, "--secret=SECRET_MARKER"},
		{"verify", "--receipt", filepath.Join(dir, "SECRET_MARKER"), "--policy", policyPath},
		{"sign", "--event", input, "--key-file", key, "--agent-id", id(), "--out", filepath.Join(archive, "wrong.json")},
	}
	for _, args := range failures {
		code, value, diagnostic := call(args...)
		if code == 0 || value != nil || diagnostic != "workflow_operation_failed\n" {
			t.Fatal(code, value, diagnostic)
		}
	}
	after, _ := os.ReadFile(receipt)
	if !bytes.Equal(before, after) {
		t.Fatal("existing receipt modified")
	}
	if code = run([]string{"verify", "--receipt", receipt, "--policy", policyPath}, brokenWriter{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("ignored output failure")
	}
	if err := os.WriteFile(artifact, []byte("altered model"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, _ = call("verify", "--receipt", receipt, "--policy", policyPath, "--artifact", artifact)
	if code == 0 {
		t.Fatal("artifact mismatch accepted")
	}
	e.ArtifactSHA256 = strings.Repeat("f", 64)
	e.ArtifactSize = "0"
	write(input, e)
	code, _, _ = call("sign", "--event", input, "--key-file", key, "--agent-id", agent, "--out", filepath.Join(archive, "unchecked.json"))
	if code == 0 {
		t.Fatal("accepted caller supplied artifact digest")
	}
}

func TestCLIHelpAndArguments(t *testing.T) {
	var out, diagnostic bytes.Buffer
	if run([]string{"--help"}, &out, &diagnostic) != 0 || !strings.Contains(out.String(), "verify-set") {
		t.Fatal("help")
	}
	for _, args := range [][]string{nil, {"unknown"}, {"sign"}, {"verify"}, {"verify-set"}, {"keygen"}, {"verify", "--policy", "x", "position-secret"}} {
		out.Reset()
		diagnostic.Reset()
		if run(args, &out, &diagnostic) == 0 || out.Len() != 0 || diagnostic.String() != "workflow_operation_failed\n" {
			t.Fatal("invalid arguments")
		}
	}
	if run([]string{"--help"}, brokenWriter{}, &diagnostic) == 0 {
		t.Fatal("ignored help output failure")
	}
}
