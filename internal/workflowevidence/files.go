package workflowevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/signingkey"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

// openRegular rejects symlink/FIFO/device inputs, including replacements between
// inspection and open. O_NONBLOCK ensures a racing FIFO cannot hang the CLI.
func openRegular(path string, private bool) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || (private && before.Mode().Perm()&0077 != 0) {
		return nil, ErrIO
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrIO
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || (private && after.Mode().Perm()&0077 != 0) {
		f.Close()
		return nil, ErrIO
	}
	return f, nil
}

func ReadFile(path string) ([]byte, error) {
	f, err := openRegular(path, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readBounded(f, MaxBytes)
}

func readBounded(f *os.File, limit int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil || info.Size() > limit {
		return nil, ErrIO
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, ErrIO
	}
	return raw, nil
}

// ReadSigner reads a private key file: an Apostille JSON key file (Ed25519 or
// ML-DSA-65) or the legacy classical raw 32-byte Ed25519 seed. Any other
// content is ErrInvalid, and errors never carry file bytes.
func ReadSigner(path string) (*core.Signer, error) {
	f, err := openRegular(path, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := readBounded(f, signingkey.MaxBytes)
	defer clear(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	signer, err := signingkey.Parse(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	return signer, nil
}

// WriteExclusive never truncates an existing output or follows its symlink.
func WriteExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrIO
	}
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(path)
		return ErrIO
	}
	return nil
}

// GenerateKey writes a new ML-DSA-65 (Core 0.3) JSON key file exclusively with
// mode 0600 and returns its key ID.
func GenerateKey(path string) (string, error) {
	file, err := core.GenerateMLDSAKeyFile("")
	if err != nil {
		return "", ErrIO
	}
	raw, err := json.Marshal(file)
	defer clear(raw)
	if err != nil {
		return "", ErrInvalid
	}
	raw = append(raw, '\n')
	if err = WriteExclusive(path, raw); err != nil {
		return "", err
	}
	return file.KeyID, nil
}

func HashFile(path string) (string, string, error) {
	f, err := openRegular(path, false)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || before.Size() < 0 || before.Size() == 1<<63-1 {
		return "", "", ErrIO
	}
	h := sha256.New()
	// Bound reading to the inspected size plus one byte, even if a producer
	// keeps appending. The digest binds only the bytes observed by this read.
	n, err := io.Copy(h, io.LimitReader(f, before.Size()+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", "", ErrIO
	}
	return hex.EncodeToString(h.Sum(nil)), strconv.FormatInt(n, 10), nil
}

func BindArtifact(event Event, path string) (Event, error) {
	if event.Validate() != nil || event.ArtifactSHA256 != "" || event.ArtifactSize != "" || (event.EventType != "model_released" && event.EventType != "deployment_accepted") {
		return Event{}, ErrInvalid
	}
	hash, size, err := HashFile(path)
	if err != nil {
		return Event{}, err
	}
	event.ArtifactSHA256, event.ArtifactSize = hash, size
	return event, nil
}

func MatchArtifact(v Verification, path string) (Verification, error) {
	if !v.Valid || v.Event.ArtifactSHA256 == "" {
		return Verification{}, ErrInvalid
	}
	hash, size, err := HashFile(path)
	if err != nil || hash != v.Event.ArtifactSHA256 || size != v.Event.ArtifactSize {
		return Verification{}, ErrInvalid
	}
	v.ArtifactBinding = "matched"
	return v, nil
}

func VerifyDirectory(path string, policy Policy) (SetVerification, error) {
	return VerifyDirectoryWith(path, policy, VerifyOptions{})
}

// VerifyDirectoryWith is VerifyDirectory with explicit options.
func VerifyDirectoryWith(path string, policy Policy, opts VerifyOptions) (SetVerification, error) {
	var out SetVerification
	if policy.Validate() != nil {
		return out, ErrInvalid
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return out, ErrIO
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return out, ErrIO
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return out, ErrIO
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return out, ErrIO
	}
	var verified []Verification
	for {
		entries, readErr := dir.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return out, ErrIO
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if len(verified) >= MaxRecords {
				return out, ErrInvalid
			}
			before, err := root.Lstat(entry.Name())
			if err != nil || !before.Mode().IsRegular() {
				return out, ErrIO
			}
			f, err := root.OpenFile(entry.Name(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return out, ErrIO
			}
			after, err := f.Stat()
			if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
				f.Close()
				return out, ErrIO
			}
			raw, err := readBounded(f, MaxBytes)
			f.Close()
			if err != nil {
				return out, err
			}
			v, err := VerifyWith(raw, policy, opts)
			if err != nil {
				return out, err
			}
			verified = append(verified, v)
		}
		if readErr == io.EOF {
			break
		}
	}
	return verifySetValues(verified, policy)
}
