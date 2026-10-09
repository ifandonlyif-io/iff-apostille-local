package signingkey

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func mldsaFile(t *testing.T) (core.KeyFile, []byte) {
	t.Helper()
	file, err := core.GenerateMLDSAKeyFile("test")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	return file, raw
}

func TestParseAcceptsBothFormats(t *testing.T) {
	file, raw := mldsaFile(t)
	signer, err := Parse(raw)
	if err != nil || signer.KeyID() != file.KeyID || !PostQuantum(signer) || signer.NaturalProtocol() != core.Protocol03 {
		t.Fatal("ML-DSA-65 key file", err)
	}
	seed := bytes.Repeat([]byte{9}, 32)
	legacy, err := Parse(seed)
	if err != nil || PostQuantum(legacy) || legacy.NaturalProtocol() != core.Protocol {
		t.Fatal("legacy raw seed", err)
	}
	// An Ed25519 JSON key file is the classical key in the shared format.
	ed, err := core.NewSigner("CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk")
	if err != nil {
		t.Fatal(err)
	}
	edFile, _ := json.Marshal(core.KeyFile{Protocol: core.Protocol, KeyID: ed.KeyID(), PublicKey: ed.PublicKey(), Seed: "CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk"})
	parsed, err := Parse(edFile)
	if err != nil || PostQuantum(parsed) || parsed.KeyID() != legacy.KeyID() {
		t.Fatal("Ed25519 JSON key file", err)
	}
}

func TestParseRejectsMalformedWithoutLeakingKeyMaterial(t *testing.T) {
	file, raw := mldsaFile(t)
	mutate := func(f func(*core.KeyFile)) []byte {
		c := file
		f(&c)
		out, _ := json.Marshal(c)
		return out
	}
	cases := map[string][]byte{
		"empty":             nil,
		"31 bytes":          bytes.Repeat([]byte{1}, 31),
		"33 bytes":          bytes.Repeat([]byte{1}, 33),
		"truncated json":    raw[:len(raw)/2],
		"trailing garbage":  append(append([]byte{}, raw...), []byte("x")...),
		"oversized":         append([]byte(strings.Repeat(" ", MaxBytes)), raw...),
		"wrong protocol":    mutate(func(k *core.KeyFile) { k.Protocol = "https://example.invalid/other" }),
		"key id mismatch":   mutate(func(k *core.KeyFile) { k.KeyID = "sha256:" + strings.Repeat("0", 64) }),
		"public mismatch":   mutate(func(k *core.KeyFile) { k.PublicKey = strings.Repeat("A", len(k.PublicKey)) }),
		"bad seed encoding": mutate(func(k *core.KeyFile) { k.Seed = strings.Repeat("!", len(k.Seed)) }),
		"short seed":        mutate(func(k *core.KeyFile) { k.Seed = k.Seed[:20] }),
		"bad role":          mutate(func(k *core.KeyFile) { k.Role = "a\nb" }),
		"unknown field":     bytes.Replace(raw, []byte(`"seed"`), []byte(`"extra":1,"seed"`), 1),
		"duplicate field":   bytes.Replace(raw, []byte(`"seed"`), []byte(`"role":"x","seed"`), 1),
		"json array":        []byte(`[` + strings.Repeat("1,", 20) + `1]`),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			signer, err := Parse(input)
			if err == nil || signer != nil {
				t.Fatal("malformed key accepted")
			}
			if err != ErrInvalid || strings.Contains(err.Error(), file.Seed) || strings.Contains(err.Error(), file.PublicKey) {
				t.Fatalf("error leaks detail: %q", err)
			}
		})
	}
}
