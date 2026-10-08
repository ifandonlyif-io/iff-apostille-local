package main

import (
	"bytes"
	"encoding/base64"
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
	dir := t.TempDir()
	seed := bytes.Repeat([]byte{55}, 32)
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, seed, 0600); err != nil {
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
	signer, err := core.NewSigner(base64.RawURLEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	return artifactPath, bundlePath, signer.KeyID()
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
