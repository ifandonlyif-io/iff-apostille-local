package workflowevidence

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func mldsaSigner(t *testing.T) *core.Signer {
	t.Helper()
	key := filepath.Join(t.TempDir(), "key.json")
	if _, err := GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	signer, err := ReadSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestGenerateKeyIsMLDSA65JSONAndSignsCore03(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key.json")
	pin, err := GenerateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(key)
	var file core.KeyFile
	if json.Unmarshal(raw, &file) != nil || file.Protocol != core.Protocol03 || file.KeyID != pin {
		t.Fatal("not an ML-DSA-65 key file")
	}
	signer, err := ReadSigner(key)
	if err != nil || signer.KeyID() != pin || signer.NaturalProtocol() != core.Protocol03 {
		t.Fatal(err)
	}
	e, _, p := fixture(t)
	p.Producers[0].KeyID = pin
	rawRecord := signed(t, e, signer)
	var record Record
	if json.Unmarshal(rawRecord, &record) != nil || record.Bundle.Protocol != core.Protocol03 || record.Bundle.Statement.Protocol != core.Protocol03 {
		t.Fatal("bundle protocol does not match the signed statement")
	}
	v, err := Verify(rawRecord, p)
	if err != nil || !v.Valid || v.CoreProtocol != core.Protocol03 {
		t.Fatalf("%+v %v", v, err)
	}
	v, err = VerifyWith(rawRecord, p, VerifyOptions{RequirePostQuantum: true})
	if err != nil || v.CoreProtocol != core.Protocol03 {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestLegacySeedStillSignsCore01AndRequirePQRejectsIt(t *testing.T) {
	e, signer, p := fixture(t) // raw-seed-derived Ed25519 signer
	raw := signed(t, e, signer)
	var record Record
	if json.Unmarshal(raw, &record) != nil || record.Bundle.Protocol != core.Protocol {
		t.Fatal("legacy key must sign Core 0.1")
	}
	v, err := Verify(raw, p)
	if err != nil || v.CoreProtocol != core.Protocol {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err = VerifyWith(raw, p, VerifyOptions{RequirePostQuantum: true}); err == nil {
		t.Fatal("Core 0.1 record accepted under require-post-quantum")
	}
	if _, err = VerifySetWith([][]byte{raw}, p, VerifyOptions{RequirePostQuantum: true}); err == nil {
		t.Fatal("Core 0.1 set accepted under require-post-quantum")
	}
}

func TestMixedVersionSetVerifiesAndReportsProtocols(t *testing.T) {
	e, legacy, p := fixture(t)
	pq := mldsaSigner(t)
	p.Producers = append(p.Producers, Producer{AgentID: e.AgentID, KeyID: pq.KeyID(), EventTypes: p.Producers[0].EventTypes})
	archive := t.TempDir()
	var records [][]byte
	for i := 1; i <= 3; i++ {
		e.EventID, _ = core.NewID()
		e.Sequence, e.Round = strconv.Itoa(i), strconv.Itoa(i)
		signer := legacy
		if i%2 == 0 {
			signer = pq
		}
		raw := signed(t, e, signer)
		records = append(records, raw)
		if err := os.WriteFile(filepath.Join(archive, e.EventID+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{core.Protocol, core.Protocol03}
	set, err := VerifyDirectory(archive, p)
	if err != nil || set.RecordCount != 3 || strings.Join(set.CoreProtocols, ",") != strings.Join(want, ",") {
		t.Fatalf("%+v %v", set, err)
	}
	if set, err = VerifySet(records, p); err != nil || len(set.CoreProtocols) != 2 {
		t.Fatalf("%+v %v", set, err)
	}
	if _, err = VerifyDirectoryWith(archive, p, VerifyOptions{RequirePostQuantum: true}); err == nil {
		t.Fatal("mixed archive accepted under require-post-quantum")
	}
	empty, err := VerifyDirectory(t.TempDir(), p)
	if err != nil || empty.CoreProtocols == nil {
		t.Fatal("empty set must report an empty protocol list", err)
	}
}

func TestReadSignerRejectsMalformedKeyFilesWithoutKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if _, err := GenerateKey(good); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(good)
	var file core.KeyFile
	_ = json.Unmarshal(raw, &file)
	cases := map[string][]byte{
		"truncated": raw[:len(raw)-40],
		"padding":   append(bytes.TrimSpace(raw), bytes.Repeat([]byte(" "), 8<<10)...),
		"swapped":   bytes.Replace(raw, []byte(file.KeyID), []byte("sha256:"+strings.Repeat("0", 64)), 1),
		"text":      []byte("not a key file at all, but longer than a seed"),
	}
	for name, content := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadSigner(path)
		if err == nil || strings.Contains(err.Error(), file.Seed) || strings.Contains(err.Error(), path) {
			t.Fatal(name, "accepted or leaked", err)
		}
	}
	// Permissions are still enforced for the JSON format.
	if err := os.Chmod(good, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSigner(good); err == nil {
		t.Fatal("group-readable key file accepted")
	}
}

func TestSignWithMLDSAKeySucceeds(t *testing.T) {
	e, _, _ := fixture(t)
	if _, err := Sign(e, mldsaSigner(t), e.AgentID, time.Now()); err != nil {
		t.Fatal(err)
	}
}
