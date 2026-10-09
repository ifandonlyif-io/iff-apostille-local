package workflowevidence

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/signingkey"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func fixture(t *testing.T) (Event, *core.Signer, Policy) {
	t.Helper()
	signer, err := core.NewSigner(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	id := func() string {
		v, err := core.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	e := Event{Schema: EventSchema, EvidenceScope: Scope, ProjectID: id(), JobID: id(), AgentID: id(), EventID: id(), ConfigurationID: id(), ModelID: id(), Sequence: "1", Round: "1", EventType: "work_completed", Framework: "flower", FrameworkVersion: "1.22.0"}
	p := Policy{Schema: PolicySchema, ProjectID: e.ProjectID, JobID: e.JobID, Producers: []Producer{{AgentID: e.AgentID, KeyID: signer.KeyID(), EventTypes: []string{"configuration_approved", "work_completed", "work_failed", "work_cancelled", "model_released", "deployment_accepted"}}}}
	return e, signer, p
}

func signed(t *testing.T, e Event, signer *core.Signer) []byte {
	t.Helper()
	r, err := Sign(e, signer, e.AgentID, time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := core.Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSignVerifyAndPolicy(t *testing.T) {
	e, signer, p := fixture(t)
	raw := signed(t, e, signer)
	v, err := Verify(raw, p)
	if err != nil || !v.Valid || v.EventType != "work_completed" || v.ArtifactBinding != "not_bound" || v.ActualExecution != "unknown" || v.ContentTruth != "unknown" || v.CurrentAuthorization != "unknown" {
		t.Fatalf("verification %+v %v", v, err)
	}
	for _, field := range []string{"project", "job", "key", "agent", "permission"} {
		t.Run(field, func(t *testing.T) {
			_, _, bad := fixture(t)
			bad.ProjectID, bad.JobID = p.ProjectID, p.JobID
			bad.Producers[0] = Producer{AgentID: e.AgentID, KeyID: signer.KeyID(), EventTypes: []string{"work_completed"}}
			switch field {
			case "project":
				bad.ProjectID, _ = core.NewID()
			case "job":
				bad.JobID, _ = core.NewID()
			case "key":
				bad.Producers[0].KeyID = "sha256:" + strings.Repeat("0", 64)
			case "agent":
				bad.Producers[0].AgentID, _ = core.NewID()
			case "permission":
				bad.Producers[0].EventTypes = []string{"model_released"}
			}
			if _, err := Verify(raw, bad); err == nil {
				t.Fatal("accepted incompatible policy")
			}
		})
	}
	for _, state := range []string{"work_failed", "work_cancelled"} {
		e.EventType = state
		v, err = Verify(signed(t, e, signer), p)
		if err != nil || v.EventType != state {
			t.Fatalf("failure became success: %+v %v", v, err)
		}
	}
	other, _ := core.NewID()
	if _, err = Sign(e, signer, other, time.Now()); err == nil {
		t.Fatal("accepted mismatched agent")
	}
}

func TestTamperAndStrictRecord(t *testing.T) {
	e, signer, p := fixture(t)
	raw := signed(t, e, signer)
	for name, altered := range map[string][]byte{
		"tampered_event":     bytes.Replace(raw, []byte("work_completed"), []byte("work_cancelled"), 1),
		"case_alias":         bytes.Replace(raw, []byte(`"event":`), []byte(`"Event":`), 1),
		"bundle_alias":       bytes.Replace(raw, []byte(`"statement":`), []byte(`"Statement":`), 1),
		"duplicate":          append([]byte(`{"event":{},`), raw[1:]...),
		"noncanonical_event": bytes.Replace(raw, []byte(`"event":{`), []byte(`"event":{ `), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(altered, p); err == nil {
				t.Fatal("accepted tamper")
			}
		})
	}
	var record Record
	if json.Unmarshal(raw, &record) != nil {
		t.Fatal("fixture")
	}
	// Even a valid signature cannot claim a different agent in its event.
	e.AgentID, _ = core.NewID()
	eventRaw, _ := core.Canonical(e)
	statement, err := core.CreateStatement(bytes.NewReader(eventRaw), "application/json", signer, nil, p.Producers[0].AgentID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	record.Event, record.Bundle.Statement = eventRaw, statement
	forged, _ := core.Canonical(record)
	if _, err := Verify(forged, p); err == nil {
		t.Fatal("accepted event/core identity mismatch")
	}
}

func TestStrictEventAndPolicy(t *testing.T) {
	e, _, p := fixture(t)
	raw, _ := core.Canonical(e)
	for name, data := range map[string][]byte{
		"unknown":   append([]byte(`{"metrics":{},`), raw[1:]...),
		"duplicate": append([]byte(`{"schema":"x",`), raw[1:]...),
		"case":      bytes.Replace(raw, []byte(`"sequence"`), []byte(`"Sequence"`), 1),
		"null":      bytes.Replace(raw, []byte(`"artifact_size":""`), []byte(`"artifact_size":null`), 1),
		"number":    bytes.Replace(raw, []byte(`"sequence":"1"`), []byte(`"sequence":1`), 1),
		"missing":   bytes.Replace(raw, []byte(`"artifact_size":"",`), nil, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseEvent(data); err == nil {
				t.Fatal("accepted shape")
			}
		})
	}
	for _, mutate := range []func(*Event){
		func(e *Event) { e.Sequence = "01" }, func(e *Event) { e.Sequence = "9007199254740992" }, func(e *Event) { e.Sequence = "0" },
		func(e *Event) { e.Framework = "flower/private" }, func(e *Event) { e.FrameworkVersion = "main" }, func(e *Event) { e.Round = "" },
		func(e *Event) { e.EventType = "configuration_approved" }, func(e *Event) { e.ArtifactSHA256 = strings.Repeat("a", 64); e.ArtifactSize = "0" },
		func(e *Event) { e.AgentID = strings.ToUpper(e.AgentID) },
	} {
		bad := e
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid event %+v", bad)
		}
	}
	policyRaw, _ := core.Canonical(p)
	for name, data := range map[string][]byte{
		"case":      bytes.Replace(policyRaw, []byte(`"key_id"`), []byte(`"Key_ID"`), 1),
		"null":      bytes.Replace(policyRaw, []byte(`"event_types":[`), []byte(`"event_types":null,"ignored":[`), 1),
		"duplicate": append([]byte(`{"job_id":"x",`), policyRaw[1:]...),
	} {
		t.Run("policy_"+name, func(t *testing.T) {
			if _, err := ParsePolicy(data); err == nil {
				t.Fatal("accepted policy shape")
			}
		})
	}
	p.Producers = append(p.Producers, p.Producers[0])
	if p.Validate() == nil {
		t.Fatal("duplicate producer accepted")
	}
	p.Producers = p.Producers[:1]
	p.Producers[0].EventTypes = []string{"work_completed", "work_completed"}
	if p.Validate() == nil {
		t.Fatal("duplicate permission accepted")
	}
}

func TestVerifySetReplaySequenceAndRounds(t *testing.T) {
	e, signer, p := fixture(t)
	first := signed(t, e, signer)
	next := func(seq, round string) []byte {
		v := e
		v.EventID, _ = core.NewID()
		v.Sequence = seq
		v.Round = round
		return signed(t, v, signer)
	}
	for name, raws := range map[string][][]byte{
		"replay": {first, first}, "gap": {first, next("3", "2")}, "collision": {first, next("1", "2")},
		"stale_round": {first, next("2", "1")}, "missing_start": {next("2", "2")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifySet(raws, p); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	for _, raws := range [][][]byte{nil, {next("2", "2"), first}} {
		v, err := VerifySet(raws, p)
		if err != nil || !v.Valid || v.RecordCount != len(raws) || v.ArchiveCompleteness != "unknown" {
			t.Fatalf("set %+v %v", v, err)
		}
	}
	e.Sequence = "2"
	e.Round = "1"
	e.EventID, _ = core.NewID()
	e.EventType = "work_failed"
	if _, err := VerifySet([][]byte{first, signed(t, e, signer)}, p); err == nil {
		t.Fatal("two terminal states for round accepted")
	}
	e.ConfigurationID, _ = core.NewID()
	if _, err := VerifySet([][]byte{first, signed(t, e, signer)}, p); err != nil {
		t.Fatal("new configuration round rejected", err)
	}
}

func TestExplicitArtifact(t *testing.T) {
	e, signer, p := fixture(t)
	e.EventType = "model_released"
	e.Round = ""
	path := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(path, []byte("synthetic weights"), 0600); err != nil {
		t.Fatal(err)
	}
	bound, err := BindArtifact(e, path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Verify(signed(t, bound, signer), p)
	if err != nil || v.ArtifactBinding != "not_checked" {
		t.Fatalf("%+v %v", v, err)
	}
	v, err = MatchArtifact(v, path)
	if err != nil || v.ArtifactBinding != "matched" {
		t.Fatalf("%+v %v", v, err)
	}
	if err = os.WriteFile(path, []byte("changed weights"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = MatchArtifact(v, path); err == nil {
		t.Fatal("artifact tampering accepted")
	}
	if _, err = BindArtifact(bound, path); err == nil {
		t.Fatal("prepopulated binding accepted")
	}
	e.EventType = "work_completed"
	e.Round = "1"
	if _, err = BindArtifact(e, path); err == nil {
		t.Fatal("work artifact accepted")
	}
}

func TestFileBoundaries(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	pin, err := GenerateKey(key)
	if err != nil || !pinPattern.MatchString(pin) {
		t.Fatal(pin, err)
	}
	info, _ := os.Stat(key)
	if info.Mode().Perm() != 0600 || info.Size() <= signingkey.RawSeedBytes {
		t.Fatal("insecure key")
	}
	original, _ := os.ReadFile(key)
	if _, err = GenerateKey(key); err == nil {
		t.Fatal("overwrote existing key")
	}
	after, _ := os.ReadFile(key)
	if !bytes.Equal(original, after) {
		t.Fatal("key changed")
	}
	if _, err = ReadSigner(key); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "key-link")
	if err = os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadSigner(link); err == nil {
		t.Fatal("symlink key accepted")
	}
	if _, err = ReadFile(link); err == nil {
		t.Fatal("symlink file accepted")
	}
	if err = WriteExclusive(link, []byte("x")); err == nil {
		t.Fatal("symlink output accepted")
	}
	if err = os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadSigner(key); err == nil {
		t.Fatal("public key seed permissions accepted")
	}
	large := filepath.Join(dir, "large")
	if err = os.WriteFile(large, bytes.Repeat([]byte("x"), MaxBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadFile(large); err == nil {
		t.Fatal("oversized input")
	}
	e, signer, p := fixture(t)
	archive := t.TempDir()
	if err = os.WriteFile(filepath.Join(archive, ".lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := VerifyDirectory(archive, p); err != nil || result.RecordCount != 0 {
		t.Fatal("empty archive", err)
	}
	for i := 1; i <= 2; i++ {
		e.EventID, _ = core.NewID()
		e.Sequence = strconv.Itoa(i)
		e.Round = strconv.Itoa(i)
		if err = os.WriteFile(filepath.Join(archive, e.EventID+".json"), signed(t, e, signer), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := VerifyDirectory(archive, p); err != nil || result.RecordCount != 2 {
		t.Fatal("archive", err)
	}
	if err = os.Symlink(large, filepath.Join(archive, "bad.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyDirectory(archive, p); err == nil {
		t.Fatal("archive symlink accepted")
	}
}
