package evidence

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func writeMLDSAKey(t *testing.T, path string) core.KeyFile {
	t.Helper()
	file, err := core.GenerateMLDSAKeyFile("gateway")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(file)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func completeWith(t *testing.T, key string, opts ...Option) Record {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "records"), key, agentID, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, _, now := fixture(t)
	if err = s.Begin("project-a", runID); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("project-a", runID, manifest(now)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get("project-a", runID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMLDSAKeyFileSignsCore03(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key.json")
	file := writeMLDSAKey(t, key)
	r := completeWith(t, key, RequirePostQuantum())
	if r.Bundle.Protocol != core.Protocol03 || r.Bundle.Statement.Protocol != core.Protocol03 {
		t.Fatal("bundle protocol must match the Core 0.3 statement")
	}
	bundle, _ := core.Canonical(r.Bundle)
	v, err := VerifyWith(r.Manifest, bundle, file.KeyID, VerifyOptions{RequirePostQuantum: true})
	if err != nil || v.CoreProtocol != core.Protocol03 || !v.SignatureValid {
		t.Fatalf("%+v %v", v, err)
	}
	if v, err = Verify(r.Manifest, bundle, file.KeyID); err != nil || v.CoreProtocol != core.Protocol03 {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestLegacySeedStaysCore01AndRequirePQRefusesIt(t *testing.T) {
	key := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(key, bytes.Repeat([]byte{23}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	r := completeWith(t, key)
	if r.Bundle.Protocol != core.Protocol {
		t.Fatal("legacy key must sign Core 0.1")
	}
	bundle, _ := core.Canonical(r.Bundle)
	pin := r.Bundle.Statement.Signature.KeyID
	v, err := Verify(r.Manifest, bundle, pin)
	if err != nil || v.CoreProtocol != core.Protocol {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err = VerifyWith(r.Manifest, bundle, pin, VerifyOptions{RequirePostQuantum: true}); err == nil {
		t.Fatal("Core 0.1 record accepted under require-post-quantum")
	}
	if _, err = New(filepath.Join(t.TempDir(), "records"), key, agentID, RequirePostQuantum()); err == nil {
		t.Fatal("gateway accepted a classical key under require-post-quantum")
	}
}

func TestEd25519JSONKeyFileSignsCore01(t *testing.T) {
	signer, _ := core.NewSigner("CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk")
	raw, _ := json.Marshal(core.KeyFile{Protocol: core.Protocol, KeyID: signer.KeyID(), PublicKey: signer.PublicKey(), Seed: "CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk"})
	key := filepath.Join(t.TempDir(), "ed.json")
	if err := os.WriteFile(key, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if r := completeWith(t, key); r.Bundle.Protocol != core.Protocol {
		t.Fatal("Ed25519 key file must sign Core 0.1")
	}
}

func TestMalformedKeyFilesAreRefusedWithoutKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	file := writeMLDSAKey(t, good)
	raw, _ := os.ReadFile(good)
	bad := map[string][]byte{
		"truncated": raw[:len(raw)/2],
		"mismatch":  bytes.Replace(raw, []byte(file.KeyID), []byte("sha256:"+strings.Repeat("0", 64)), 1),
		"long":      bytes.Repeat([]byte("x"), 5<<10),
		"odd":       bytes.Repeat([]byte("x"), 40),
	}
	for name, content := range bad {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := New(filepath.Join(dir, "r-"+name), path, agentID)
		if err == nil || strings.Contains(err.Error(), file.Seed) || strings.Contains(err.Error(), path) {
			t.Fatal(name, err)
		}
	}
	if err := os.Chmod(good, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(dir, "r-perm"), good, agentID); err == nil {
		t.Fatal("group/world readable key file accepted")
	}
}
