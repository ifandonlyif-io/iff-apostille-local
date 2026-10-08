package evidence

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

const agentID = "11111111-1111-4111-8111-111111111111"
const runID = "22222222-2222-4222-8222-222222222222"

func fixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	seed := bytes.Repeat([]byte{23}, 32)
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, seed, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(filepath.Join(dir, "records"), key, agentID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, filepath.Join(dir, "records"), now
}

func manifest(now time.Time) Manifest {
	m := NewManifest(config.Model{ID: "synthetic-model", Revision: strings.Repeat("a", 40), ManifestSHA256: strings.Repeat("b", 64), RuntimeImage: "example/runtime@sha256:" + strings.Repeat("c", 64), Precision: "float16", Path: "/private/customer/recipe"})
	m.RunID, m.GatewayVersion = runID, "0.1.0-test"
	m.StartedAt, m.CompletedAt = now.Add(-time.Second).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)
	m.FinishReason = "stop"
	return m
}

func TestConfiguredImageCanBeRecorded(t *testing.T) {
	// A catalog accepted at startup must not cause every receipt to fail later.
	for _, image := range []string{"example/runtime@sha256:" + strings.Repeat("c", 64), "registry.example:5000/team/runtime:v1@sha256:" + strings.Repeat("c", 64), strings.Repeat("a", 191) + "@sha256:" + strings.Repeat("c", 64)} {
		s, _, now := fixture(t)
		m := manifest(now)
		m.ConfiguredModel.RuntimeImage = image
		if !config.ValidRuntimeImage(image) {
			t.Fatal("test image violates configuration contract")
		}
		if err := s.Begin("project-a", runID); err != nil {
			t.Fatal(err)
		}
		if err := s.Complete("project-a", runID, m); err != nil {
			t.Fatal("configured image could not be recorded", err)
		}
	}
}

func TestSignedRecordAndIndependentPolicy(t *testing.T) {
	s, _, now := fixture(t)
	if err := s.Begin("project-a", runID); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete("project-a", runID, manifest(now)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get("project-a", runID)
	if err != nil || r.Status != "ready" {
		t.Fatalf("record: %+v, %v", r, err)
	}
	bundle, err := core.Canonical(r.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Verify(r.Manifest, bundle, s.signer.KeyID())
	if err != nil || result.IssuerTrust != "unknown" || result.ProducerKeyPolicy != "matched" || result.CertificateScope != "producer_only" {
		t.Fatalf("verification: %+v, %v", result, err)
	}
	if bytes.Contains(r.Manifest, []byte("/private")) || bytes.Contains(r.Manifest, []byte("path")) || bytes.Contains(r.Manifest, []byte("prompt")) || bytes.Contains(r.Manifest, []byte("output")) {
		t.Fatal("private fields in metadata")
	}
	if _, err = Verify(r.Manifest, bundle, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong producer pin accepted")
	}
	tampered := bytes.Replace(r.Manifest, []byte("synthetic-model"), []byte("different-model"), 1)
	if _, err = Verify(tampered, bundle, s.signer.KeyID()); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	bad := *r.Bundle
	bad.Statement.Signature.Value = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	badBytes, _ := core.Canonical(bad)
	if _, err = Verify(r.Manifest, badBytes, s.signer.KeyID()); err == nil {
		t.Fatal("tampered signature accepted")
	}
	if _, err = s.Get("project-b", runID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-project read", err)
	}
	if err = s.Fail("project-a", runID); !errors.Is(err, ErrConflict) {
		t.Fatal("completed record overwritten", err)
	}
	if err = s.Complete("project-a", runID, manifest(now)); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate completion accepted", err)
	}
}

func TestExpiryHidesAndPurgesAllStates(t *testing.T) {
	s, dir, now := fixture(t)
	ids := []string{runID, "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"}
	for _, id := range ids {
		if err := s.Begin("project-a", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Complete("project-a", runID, manifest(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail("project-a", ids[1]); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(Retention) }
	for _, id := range ids {
		if _, err := s.Get("project-a", id); !errors.Is(err, ErrNotFound) {
			t.Fatal("expired record exposed", err)
		}
	}
	if err := s.Purge(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("records retained: %v %v", entries, err)
	}
}

func TestFailureHasNoPartialSignedData(t *testing.T) {
	s, _, now := fixture(t)
	if err := s.Begin("project-a", runID); err != nil {
		t.Fatal(err)
	}
	bad := manifest(now)
	bad.FinishReason = "cancelled"
	if err := s.Complete("project-a", runID, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("cancelled receipt accepted", err)
	}
	if err := s.Fail("project-a", runID); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get("project-a", runID)
	if err != nil || r.Status != "failed" || r.Bundle != nil || len(r.Manifest) != 0 {
		t.Fatalf("failure contains signed data: %+v %v", r, err)
	}
	if err = s.Complete("project-a", runID, manifest(now)); !errors.Is(err, ErrConflict) {
		t.Fatal("failed run completed", err)
	}
}

func TestStorageFailurePreservesPending(t *testing.T) {
	s, dir, now := fixture(t)
	if err := s.Begin("project-a", runID); err != nil {
		t.Fatal(err)
	}
	name, _ := recordName("project-a", runID)
	before, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.root.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("project-a", runID, manifest(now)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unavailable storage accepted", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pending record changed on failure", err)
	}
}

func TestCompletionIsAtomicToFilesystemReaders(t *testing.T) {
	s, dir, now := fixture(t)
	if err := s.Begin("project-a", runID); err != nil {
		t.Fatal(err)
	}
	name, _ := recordName("project-a", runID)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Error(err)
				return
			}
			var r storedRecord
			if json.Unmarshal(raw, &r) != nil {
				t.Error("partially written JSON")
				return
			}
			if r.Record.Status == "ready" && (r.Record.Bundle == nil || len(r.Record.Manifest) == 0) {
				t.Error("partial completed record")
				return
			}
			if r.Record.Status != "ready" && r.Record.Status != "pending" {
				t.Error("unexpected record state")
				return
			}
		}
	}()
	close(start)
	if err := s.Complete("project-a", runID, manifest(now)); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file leaked: %v %v", entries, err)
	}
}

func TestPrivateKeyAndDirectoryBoundaries(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	seed := bytes.Repeat([]byte{4}, 32)
	if err := os.WriteFile(key, seed, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(dir, "records"), key, agentID); err == nil {
		t.Fatal("world-readable key accepted")
	}
	if err := os.Chmod(key, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "key-link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(dir, "records"), link, agentID); err == nil {
		t.Fatal("symlink key accepted")
	}
	if err := os.WriteFile(key, append(seed, 1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(dir, "records"), key, agentID); err == nil {
		t.Fatal("oversized key accepted")
	}
	if _, err := recordName("../project", runID); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestRecordSymlinksAreNotFollowed(t *testing.T) {
	s, dir, _ := fixture(t)
	name, _ := recordName("project-a", runID)
	outside := filepath.Join(t.TempDir(), "record")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("project-a", runID); !errors.Is(err, ErrUnavailable) {
		t.Fatal("record symlink followed", err)
	}
}

func TestRestartRecoversPendingAndExcludesSecondOwner(t *testing.T) {
	s, dir, _ := fixture(t)
	s.now = time.Now
	key := filepath.Join(filepath.Dir(dir), "key")
	if other, err := New(dir, key, agentID); err == nil {
		other.Close()
		t.Fatal("second writer admitted")
	}
	readyID := "33333333-3333-4333-8333-333333333333"
	for _, id := range []string{runID, readyID} {
		if err := s.Begin("project-a", id); err != nil {
			t.Fatal(err)
		}
	}
	m := manifest(time.Now().UTC())
	m.RunID = readyID
	if err := s.Complete("project-a", readyID, m); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get("project-a", readyID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir, key, agentID)
	if err != nil {
		t.Fatal("restart failed", err)
	}
	defer restarted.Close()
	recovered, err := restarted.Get("project-a", runID)
	if err != nil || recovered.Status != "failed" || recovered.Bundle != nil || len(recovered.Manifest) != 0 {
		t.Fatalf("interrupted run not recovered: %+v %v", recovered, err)
	}
	after, err := restarted.Get("project-a", readyID)
	if err != nil || after.Status != "ready" || !bytes.Equal(before.Manifest, after.Manifest) {
		t.Fatal("completed record changed during recovery", err)
	}
}

func TestFullDiskDoesNotExposePendingAfterTerminalFailure(t *testing.T) {
	s, dir, _ := fixture(t)
	s.now = time.Now
	if err := s.Begin("project-a", runID); err != nil {
		t.Fatal(err)
	}
	// Fault injection at the filesystem allocation boundary keeps reads working,
	// reproducing ENOSPC without changing process-wide limits or filling a disk.
	s.createTemp = func(string) (*os.File, error) { return nil, syscall.ENOSPC }
	if err := s.Complete("project-a", runID, manifest(time.Now().UTC())); !errors.Is(err, ErrUnavailable) {
		t.Fatal("completion did not report disk failure", err)
	}
	if err := s.Fail("project-a", runID); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed transition did not report disk failure", err)
	}
	if rec, err := s.Get("project-a", runID); !errors.Is(err, ErrUnavailable) || rec.Status == "pending" {
		t.Fatal("terminal failure looks like live inference", rec, err)
	}
	if _, err := s.Get("project-b", runID); !errors.Is(err, ErrNotFound) {
		t.Fatal("override disclosed another project's run", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir, filepath.Join(filepath.Dir(dir), "key"), agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if rec, err := restarted.Get("project-a", runID); err != nil || rec.Status != "failed" {
		t.Fatal("restart did not recover interrupted terminal write", rec, err)
	}
}

func TestPurgeRemovesOnlyExpiredOwnedTemporaryFiles(t *testing.T) {
	s, dir, now := fixture(t)
	oldName := ".record-" + runID + ".tmp"
	recentName := ".record-33333333-3333-4333-8333-333333333333.tmp"
	for _, name := range []string{oldName, recentName, ".record-admin.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-Retention)
	if err := os.Chtimes(filepath.Join(dir, oldName), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, recentName), now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, oldName)); !os.IsNotExist(err) {
		t.Fatal("expired temporary retained", err)
	}
	for _, name := range []string{recentName, ".record-admin.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("unexpired or unrelated file removed", name, err)
		}
	}
}
