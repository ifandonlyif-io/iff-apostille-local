package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func exportedRecord(t *testing.T) (string, string, string) {
	t.Helper()
	return exportedRecordWithKey(t, bytes.Repeat([]byte{55}, 32))
}

// exportedRecordWithKey signs one record with the given key file contents.
func exportedRecordWithKey(t *testing.T, keyContents []byte) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, keyContents, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := evidence.New(filepath.Join(dir, "records"), key, "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runID := "22222222-2222-4222-8222-222222222222"
	m := evidence.NewManifest(config.Model{ID: "synthetic", Revision: strings.Repeat("a", 40), ManifestSHA256: strings.Repeat("b", 64), RuntimeImage: "example/runtime@sha256:" + strings.Repeat("c", 64), Precision: "bfloat16"})
	m.RunID, m.GatewayVersion, m.FinishReason = runID, "0.1.0-test", "length"
	now := time.Now().UTC()
	m.StartedAt = now.Add(-time.Second).Format(time.RFC3339Nano)
	m.CompletedAt = now.Format(time.RFC3339Nano)
	if err = s.Begin("project-a", runID); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("project-a", runID, m); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get("project-a", runID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := core.Canonical(r.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	artifactPath, bundlePath := filepath.Join(dir, "artifact.json"), filepath.Join(dir, "bundle.json")
	if err = os.WriteFile(artifactPath, r.Manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(bundlePath, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	return artifactPath, bundlePath, r.Bundle.Statement.Signature.KeyID
}

func TestOfflineVerifyAuthenticatesProducerSeparately(t *testing.T) {
	artifact, bundle, pin := exportedRecord(t)
	var out bytes.Buffer
	if err := run([]string{"--artifact", artifact, "--bundle", bundle, "--producer-pin", pin}, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Valid bool `json:"valid"`
		evidence.Verification
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Valid || !result.ArtifactMatches || result.ProducerKeyPolicy != "matched" || result.IssuerTrust != "unknown" || result.EvidenceScope != "run_metadata_only" {
		t.Fatalf("unexpected verification: %+v", result)
	}
	out.Reset()
	if err := run([]string{"--artifact", artifact, "--bundle", bundle, "--producer-pin", "sha256:" + strings.Repeat("0", 64)}, &out); err == nil {
		t.Fatal("wrong pin accepted")
	}
	if !strings.Contains(out.String(), `"valid":false`) {
		t.Fatal("failure was not machine-readable")
	}
}

func TestVerifierRequiresPinAndRejectsTampering(t *testing.T) {
	artifact, bundle, pin := exportedRecord(t)
	if err := run([]string{"--artifact", artifact, "--bundle", bundle}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing independent pin accepted")
	}
	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(artifact, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"--artifact", artifact, "--bundle", bundle, "--producer-pin", pin}, &bytes.Buffer{}); err == nil {
		t.Fatal("different artifact bytes accepted")
	}
}

func TestVerifierRejectsSymlinksAndOversizedFiles(t *testing.T) {
	artifact, _, _ := exportedRecord(t)
	link := filepath.Join(t.TempDir(), "artifact-link")
	if err := os.Symlink(artifact, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(link, evidence.MaxRecordBytes); err == nil {
		t.Fatal("symlink accepted")
	}
	large := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(large, make([]byte, evidence.MaxRecordBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(large, evidence.MaxRecordBytes); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestRequirePostQuantumFlag(t *testing.T) {
	keyFile, err := core.GenerateMLDSAKeyFile("test")
	if err != nil {
		t.Fatal(err)
	}
	keyJSON, _ := json.Marshal(keyFile)
	pqArtifact, pqBundle, pqPin := exportedRecordWithKey(t, keyJSON)
	var out bytes.Buffer
	if err = run([]string{"--artifact", pqArtifact, "--bundle", pqBundle, "--producer-pin", pqPin, "--require-post-quantum"}, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Valid        bool   `json:"valid"`
		CoreProtocol string `json:"core_protocol"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || !result.Valid || result.CoreProtocol != core.Protocol03 {
		t.Fatalf("unexpected output %s", out.String())
	}
	// Core 0.1 verifies by default and is rejected under the flag.
	artifact, bundle, pin := exportedRecord(t)
	out.Reset()
	if err = run([]string{"--artifact", artifact, "--bundle", bundle, "--producer-pin", pin}, &out); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || result.CoreProtocol != core.Protocol {
		t.Fatalf("unexpected output %s", out.String())
	}
	out.Reset()
	if err = run([]string{"--artifact", artifact, "--bundle", bundle, "--producer-pin", pin, "--require-post-quantum"}, &out); err == nil {
		t.Fatal("Core 0.1 record accepted under --require-post-quantum")
	}
	if strings.Contains(out.String(), `"valid":true`) {
		t.Fatalf("unexpected success output %s", out.String())
	}
}
