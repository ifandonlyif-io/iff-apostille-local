package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	workflow "github.com/ifandonlyif-io/iff-apostille-local/internal/workflowevidence"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

// TestPostQuantumDefaultsAndMixedSets drives the CLI with a generated ML-DSA-65
// key and a legacy raw seed for the same producer.
func TestPostQuantumDefaultsAndMixedSets(t *testing.T) {
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
	call := func(args ...string) (int, map[string]any) {
		t.Helper()
		var out, diagnostic bytes.Buffer
		code := run(args, &out, &diagnostic)
		var value map[string]any
		if out.Len() > 0 && json.Unmarshal(out.Bytes(), &value) != nil {
			t.Fatal("invalid JSON output")
		}
		return code, value
	}
	agent := id()
	pqKey, legacyKey := filepath.Join(dir, "pq.json"), filepath.Join(dir, "legacy")
	code, generated := call("keygen", "--out-key", pqKey, "--agent-id", agent)
	if code != 0 {
		t.Fatal("keygen failed")
	}
	pqPin := generated["producer_pin"].(string)
	raw, _ := os.ReadFile(pqKey)
	var file core.KeyFile
	if json.Unmarshal(raw, &file) != nil || file.Protocol != core.Protocol03 {
		t.Fatal("keygen must write an ML-DSA-65 JSON key file")
	}
	seed := bytes.Repeat([]byte{5}, 32)
	if err := os.WriteFile(legacyKey, seed, 0600); err != nil {
		t.Fatal(err)
	}
	legacySigner, err := workflow.ReadSigner(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	project, job := id(), id()
	write := func(path string, v any) {
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	types := []string{"configuration_approved", "work_completed"}
	policyPath := filepath.Join(dir, "policy.json")
	write(policyPath, workflow.Policy{Schema: workflow.PolicySchema, ProjectID: project, JobID: job, Producers: []workflow.Producer{
		{AgentID: agent, KeyID: pqPin, EventTypes: types}, {AgentID: agent, KeyID: legacySigner.KeyID(), EventTypes: types}}})
	config := id()
	model := id()
	receipts := map[string]string{}
	for i, key := range []string{legacyKey, pqKey} {
		e := workflow.Event{Schema: workflow.EventSchema, EvidenceScope: workflow.Scope, ProjectID: project, JobID: job, AgentID: agent, EventID: id(), ConfigurationID: config, ModelID: model, Sequence: string(rune('1' + i)), EventType: "configuration_approved", Framework: "generic", FrameworkVersion: "1.0.0"}
		input := filepath.Join(dir, "event"+e.Sequence+".json")
		write(input, e)
		out := filepath.Join(archive, e.EventID+".json")
		if code, _ := call("sign", "--event", input, "--key-file", key, "--agent-id", agent, "--out", out); code != 0 {
			t.Fatal("sign failed for", key)
		}
		receipts[key] = out
	}
	protocolOf := func(path string) string {
		code, v := call("verify", "--receipt", path, "--policy", policyPath)
		if code != 0 {
			t.Fatal("default verify failed")
		}
		return v["core_protocol"].(string)
	}
	if protocolOf(receipts[legacyKey]) != core.Protocol || protocolOf(receipts[pqKey]) != core.Protocol03 {
		t.Fatal("signed version must follow the key")
	}
	if code, _ = call("verify", "--receipt", receipts[legacyKey], "--policy", policyPath, "--require-post-quantum"); code == 0 {
		t.Fatal("Core 0.1 receipt accepted under --require-post-quantum")
	}
	if code, v := call("verify", "--receipt", receipts[pqKey], "--policy", policyPath, "--require-post-quantum"); code != 0 || v["core_protocol"] != core.Protocol03 {
		t.Fatal("Core 0.3 receipt rejected under --require-post-quantum")
	}
	code, set := call("verify-set", "--directory", archive, "--policy", policyPath)
	protocols, _ := set["core_protocols"].([]any)
	if code != 0 || set["record_count"] != float64(2) || len(protocols) != 2 || protocols[0] != core.Protocol || protocols[1] != core.Protocol03 {
		t.Fatal("mixed set must verify and list both versions", code, set)
	}
	if code, _ = call("verify-set", "--directory", archive, "--policy", policyPath, "--require-post-quantum"); code == 0 {
		t.Fatal("mixed set accepted under --require-post-quantum")
	}
}
