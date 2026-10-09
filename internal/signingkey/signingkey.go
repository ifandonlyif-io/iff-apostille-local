// Package signingkey parses the two private key formats Apostille Local reads.
//
// A key file is either an Apostille JSON key file (Ed25519 or ML-DSA-65) or the
// legacy raw 32-byte Ed25519 seed. The formats are told apart by size alone, so
// no file is ever interpreted under two formats. Errors never carry file bytes.
package signingkey

import (
	"encoding/base64"
	"errors"

	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

const (
	// RawSeedBytes is the size of a legacy raw Ed25519 seed file.
	RawSeedBytes = 32
	// MaxBytes bounds any private key file this package accepts.
	MaxBytes = core.MaxKeyFileBytes
)

// ErrInvalid is the only error Parse returns. It carries no key material.
var ErrInvalid = errors.New("invalid signing key file")

// Parse strictly parses raw key file contents. Exactly 32 bytes is a legacy
// classical (Ed25519, Core 0.1) seed; anything from 33 to MaxBytes bytes must be
// a valid Apostille JSON key file; every other size is invalid. The caller owns
// raw and should clear it after Parse returns.
func Parse(raw []byte) (*core.Signer, error) {
	switch {
	case len(raw) == RawSeedBytes:
		seed := base64.RawURLEncoding.EncodeToString(raw)
		signer, err := core.NewSigner(seed)
		if err != nil || !signer.Enabled() {
			return nil, ErrInvalid
		}
		return signer, nil
	case len(raw) > RawSeedBytes && len(raw) <= MaxBytes:
		signer, _, err := core.ParseKeyFile(raw)
		if err != nil || signer == nil || !signer.Enabled() {
			return nil, ErrInvalid
		}
		return signer, nil
	default:
		return nil, ErrInvalid
	}
}

// PostQuantum reports whether the signer signs Core 0.3 (ML-DSA-65).
func PostQuantum(signer *core.Signer) bool {
	return signer.NaturalProtocol() == core.Protocol03
}
