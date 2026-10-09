// Package evidence signs local run metadata. It never accepts inference content.
package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/signingkey"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

const (
	Schema         = "urn:apostille-local:run:0.1"
	Scope          = "run_metadata_only"
	Retention      = 24 * time.Hour
	MaxRecordBytes = 64 << 10
)

var (
	ErrNotFound     = errors.New("record_not_found")
	ErrUnavailable  = errors.New("record_unavailable")
	ErrInvalid      = errors.New("invalid_record")
	ErrConflict     = errors.New("record_conflict")
	keyPinPattern   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	hexPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	revisionPattern = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
	filePattern     = regexp.MustCompile(`^[a-f0-9]{64}\.json$`)
)

// ConfiguredModel reports administrator configuration, not measured GPU execution.
// Keep this explicit subset: config.Model also contains a private filesystem path.
type ConfiguredModel struct {
	ID             string `json:"id"`
	Revision       string `json:"revision"`
	ManifestSHA256 string `json:"manifest_sha256"`
	RuntimeImage   string `json:"runtime_image"`
	Precision      string `json:"precision"`
}

type Manifest struct {
	Schema          string          `json:"schema"`
	EvidenceScope   string          `json:"evidence_scope"`
	RunID           string          `json:"run_id"`
	ConfiguredModel ConfiguredModel `json:"configured_model"`
	GatewayVersion  string          `json:"gateway_version"`
	StartedAt       string          `json:"started_at"`
	CompletedAt     string          `json:"completed_at"`
	FinishReason    string          `json:"finish_reason"`
	Status          string          `json:"status"`
}

func NewManifest(model config.Model) Manifest {
	return Manifest{Schema: Schema, EvidenceScope: Scope, Status: "completed", ConfiguredModel: ConfiguredModel{
		ID: model.ID, Revision: model.Revision, ManifestSHA256: model.ManifestSHA256,
		RuntimeImage: model.RuntimeImage, Precision: model.Precision,
	}}
}

type Record struct {
	Status   string          `json:"receipt_status"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
	Bundle   *core.Bundle    `json:"bundle,omitempty"`
}

type storedRecord struct {
	Version   int       `json:"version"`
	ProjectID string    `json:"project_id"`
	RunID     string    `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
	Record    Record    `json:"record"`
}

// Store supports one gateway process owning its private directory. Complete and
// Fail are terminal transitions; retries cannot overwrite signed records.
type Store struct {
	mu         sync.Mutex
	root       *os.Root
	lock       *os.File
	signer     *core.Signer
	agentID    string
	now        func() time.Time
	createTemp func(string) (*os.File, error)
	// A failed terminal write must not leave a live-looking pending response.
	unavailable map[string]time.Time
}

// Option adjusts how New treats the signing key.
type Option func(*options)

type options struct{ requirePostQuantum bool }

// RequirePostQuantum makes New refuse a key that does not sign Core 0.3
// (ML-DSA-65), such as a classical Ed25519 key or the legacy raw seed.
func RequirePostQuantum() Option { return func(o *options) { o.requirePostQuantum = true } }

// New opens the store. The key file is an Apostille JSON key file (Ed25519 or
// ML-DSA-65) or the legacy classical raw 32-byte Ed25519 seed; records are
// signed in the key's natural Core version.
func New(dir, keyFile, agentID string, opts ...Option) (*Store, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if dir == "" || !core.ValidID(agentID) {
		return nil, ErrInvalid
	}
	raw, err := readKeyFile(keyFile)
	if err != nil {
		return nil, err
	}
	signer, err := signingkey.Parse(raw)
	clear(raw)
	if err != nil || (o.requirePostQuantum && !signingkey.PostQuantum(signer)) {
		return nil, ErrInvalid
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrUnavailable
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrUnavailable
	}
	lock, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, ErrUnavailable
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		root.Close()
		return nil, ErrUnavailable
	}
	s := &Store{root: root, lock: lock, signer: signer, agentID: agentID, now: time.Now, unavailable: make(map[string]time.Time)}
	s.createTemp = func(name string) (*os.File, error) {
		return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}
	if s.Purge() != nil || s.recoverPending() != nil {
		s.Close()
		return nil, ErrUnavailable
	}
	return s, nil
}

func readKeyFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() < signingkey.RawSeedBytes || before.Size() > signingkey.MaxBytes {
		return nil, ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(f, signingkey.MaxBytes+1))
	if err != nil || len(raw) > signingkey.MaxBytes {
		clear(raw)
		return nil, ErrInvalid
	}
	return raw, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.root.Close(), s.lock.Close())
}

func recordName(projectID, runID string) (string, error) {
	if !config.ValidID(projectID) || !core.ValidID(runID) {
		return "", ErrInvalid
	}
	return core.Hash([]byte(projectID+"\x00"+runID)) + ".json", nil
}

func (s *Store) Begin(projectID, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, err := recordName(projectID, runID)
	if err != nil {
		return err
	}
	if _, err = s.root.Lstat(name); err == nil {
		return ErrConflict
	} else if !os.IsNotExist(err) {
		return ErrUnavailable
	}
	r := storedRecord{Version: 1, ProjectID: projectID, RunID: runID, CreatedAt: s.now().UTC(), Record: Record{Status: "pending"}}
	return s.write(name, r)
}

func (s *Store) Complete(projectID, runID string, manifest Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, err := recordName(projectID, runID)
	if err != nil {
		return err
	}
	if _, failed := s.unavailable[name]; failed {
		return ErrUnavailable
	}
	r, err := s.read(name, projectID, runID)
	if err != nil {
		return err
	}
	if r.Record.Status != "pending" {
		return ErrConflict
	}
	if manifest.RunID != runID || validateManifest(manifest) != nil {
		return ErrInvalid
	}
	raw, err := core.Canonical(manifest)
	if err != nil {
		return ErrInvalid
	}
	statement, err := core.CreateStatement(bytes.NewReader(raw), "application/json", s.signer, nil, s.agentID, s.now().UTC())
	if err != nil {
		return ErrUnavailable
	}
	if statement.Protocol != s.signer.NaturalProtocol() {
		return ErrUnavailable
	}
	bundle := &core.Bundle{Protocol: statement.Protocol, Statement: statement}
	bundleRaw, err := core.Canonical(bundle)
	if err != nil {
		return ErrUnavailable
	}
	if _, err = VerifyWith(raw, bundleRaw, s.signer.KeyID(), VerifyOptions{}); err != nil {
		return ErrUnavailable
	}
	r.Record = Record{Status: "ready", Manifest: raw, Bundle: bundle}
	return s.write(name, r)
}

func (s *Store) Fail(projectID, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, err := recordName(projectID, runID)
	if err != nil {
		return err
	}
	r, err := s.read(name, projectID, runID)
	if err != nil {
		return err
	}
	if r.Record.Status == "failed" {
		return nil
	}
	if r.Record.Status != "pending" {
		return ErrConflict
	}
	r.Record = Record{Status: "failed"}
	if err = s.write(name, r); err != nil {
		s.unavailable[name] = r.CreatedAt.Add(Retention)
		return err
	}
	delete(s.unavailable, name)
	return nil
}

func (s *Store) Get(projectID, runID string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, err := recordName(projectID, runID)
	if err != nil {
		return Record{}, ErrNotFound
	}
	if expiry, failed := s.unavailable[name]; failed {
		if s.now().Before(expiry) {
			return Record{}, ErrUnavailable
		}
		delete(s.unavailable, name)
	}
	r, err := s.read(name, projectID, runID)
	if err != nil {
		return Record{}, err
	}
	return r.Record, nil
}

func (s *Store) read(name, projectID, runID string) (storedRecord, error) {
	var r storedRecord
	info, err := s.root.Lstat(name)
	if os.IsNotExist(err) {
		return r, ErrNotFound
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > MaxRecordBytes {
		return r, ErrUnavailable
	}
	f, err := s.root.Open(name)
	if err != nil {
		return r, ErrUnavailable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return r, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxRecordBytes+1))
	if err != nil || len(raw) > MaxRecordBytes {
		return r, ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return r, ErrUnavailable
	}
	var extra any
	if d.Decode(&extra) != io.EOF || r.Version != 1 || r.CreatedAt.IsZero() {
		return r, ErrUnavailable
	}
	if r.ProjectID != projectID || r.RunID != runID {
		return r, ErrNotFound
	}
	if !s.now().Before(r.CreatedAt.Add(Retention)) {
		return r, ErrNotFound
	}
	switch r.Record.Status {
	case "pending", "failed":
		if len(r.Record.Manifest) != 0 || r.Record.Bundle != nil {
			return r, ErrUnavailable
		}
	case "ready":
		bundleRaw, err := core.Canonical(r.Record.Bundle)
		if err != nil || r.Record.Bundle == nil {
			return r, ErrUnavailable
		}
		// Historical records may use a previous local signing key. Authenticity
		// policy is applied by the offline verifier's independently supplied pin.
		verified, err := Verify(r.Record.Manifest, bundleRaw, r.Record.Bundle.Statement.Signature.KeyID)
		if err != nil || verified.RunID != runID {
			return r, ErrUnavailable
		}
	default:
		return r, ErrUnavailable
	}
	return r, nil
}

func (s *Store) write(name string, record storedRecord) error {
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > MaxRecordBytes {
		return ErrUnavailable
	}
	id, err := core.NewID()
	if err != nil {
		return ErrUnavailable
	}
	temp := ".record-" + id + ".tmp"
	f, err := s.createTemp(temp)
	if err != nil {
		return ErrUnavailable
	}
	defer s.root.Remove(temp)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	if err = s.root.Rename(temp, name); err != nil {
		return ErrUnavailable
	}
	// Sync the directory so the rename survives a clean filesystem recovery.
	dir, err := s.root.Open(".")
	if err != nil {
		return ErrUnavailable
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return ErrUnavailable
	}
	return nil
}

// Purge removes expired records regardless of their state. It never follows a
// symlink or recursively deletes administrator-owned directories.
func (s *Store) Purge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.root.Open(".")
	if err != nil {
		return ErrUnavailable
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return ErrUnavailable
	}
	for name, expiry := range s.unavailable {
		if !s.now().Before(expiry) {
			delete(s.unavailable, name)
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		temporary := strings.HasPrefix(name, ".record-") && strings.HasSuffix(name, ".tmp") && core.ValidID(strings.TrimSuffix(strings.TrimPrefix(name, ".record-"), ".tmp"))
		if !filePattern.MatchString(name) && !temporary {
			continue
		}
		info, err := s.root.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() {
			return ErrUnavailable
		}
		// A fixed, bounded envelope read is sufficient for retention metadata;
		// corrupt files are removed after their filesystem retention deadline.
		created := info.ModTime()
		if !temporary && info.Size() <= MaxRecordBytes {
			f, openErr := s.root.Open(entry.Name())
			if openErr != nil {
				return ErrUnavailable
			}
			var r storedRecord
			d := json.NewDecoder(io.LimitReader(f, MaxRecordBytes+1))
			if d.Decode(&r) == nil && !r.CreatedAt.IsZero() {
				created = r.CreatedAt
			}
			f.Close()
		}
		if !s.now().Before(created.Add(Retention)) {
			if s.root.Remove(entry.Name()) != nil {
				return ErrUnavailable
			}
		}
	}
	return nil
}

// Holding the directory lock guarantees that every pre-existing pending run
// belongs to an interrupted process. Recovery never manufactures a signature.
func (s *Store) recoverPending() error {
	dir, err := s.root.Open(".")
	if err != nil {
		return ErrUnavailable
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return ErrUnavailable
	}
	for _, entry := range entries {
		if !filePattern.MatchString(entry.Name()) {
			continue
		}
		info, err := s.root.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() || info.Size() > MaxRecordBytes {
			return ErrUnavailable
		}
		f, err := s.root.Open(entry.Name())
		if err != nil {
			return ErrUnavailable
		}
		var candidate storedRecord
		err = json.NewDecoder(io.LimitReader(f, MaxRecordBytes+1)).Decode(&candidate)
		f.Close()
		if err != nil {
			return ErrUnavailable
		}
		name, err := recordName(candidate.ProjectID, candidate.RunID)
		if err != nil || name != entry.Name() {
			return ErrUnavailable
		}
		r, err := s.read(name, candidate.ProjectID, candidate.RunID)
		if err != nil {
			return ErrUnavailable
		}
		if r.Record.Status == "pending" {
			r.Record = Record{Status: "failed"}
			if s.write(name, r) != nil {
				return ErrUnavailable
			}
		}
	}
	return nil
}

func validateManifest(m Manifest) error {
	model := m.ConfiguredModel
	if m.Schema != Schema || m.EvidenceScope != Scope || !core.ValidID(m.RunID) || m.Status != "completed" || (m.FinishReason != "stop" && m.FinishReason != "length" && m.FinishReason != "tool_calls") || !config.ValidID(m.GatewayVersion) || !config.ValidID(model.ID) || !revisionPattern.MatchString(model.Revision) || !hexPattern.MatchString(model.ManifestSHA256) || !config.ValidRuntimeImage(model.RuntimeImage) || (model.Precision != "float16" && model.Precision != "bfloat16") {
		return ErrInvalid
	}
	start, err := time.Parse(time.RFC3339Nano, m.StartedAt)
	if err != nil || start.IsZero() || !strings.HasSuffix(m.StartedAt, "Z") {
		return ErrInvalid
	}
	end, err := time.Parse(time.RFC3339Nano, m.CompletedAt)
	if err != nil || end.Before(start) || !strings.HasSuffix(m.CompletedAt, "Z") {
		return ErrInvalid
	}
	return nil
}

type Verification struct {
	SignatureValid    bool   `json:"signature_valid"`
	ArtifactMatches   bool   `json:"artifact_matches"`
	ProducerKeyPolicy string `json:"producer_key_policy"`
	EvidenceScope     string `json:"evidence_scope"`
	CertificateScope  string `json:"certificate_scope"`
	IssuerTrust       string `json:"issuer_trust"`
	RunID             string `json:"run_id"`
	// CoreProtocol is the Apostille Core version the record is signed under.
	// Only Core 0.3 (ML-DSA-65) signatures are post-quantum.
	CoreProtocol string `json:"core_protocol"`
}

// VerifyOptions tightens verification beyond the default of accepting every
// Core version this build knows (0.1 and 0.3).
type VerifyOptions struct {
	// RequirePostQuantum accepts only Core 0.3 (ML-DSA-65) records.
	RequirePostQuantum bool
}

// Verify is offline and authenticates an exact producer pin selected by the
// caller. Core's issuer_trust remains unknown for a producer-only statement.
func Verify(artifact, bundleRaw []byte, producerPin string) (Verification, error) {
	return VerifyWith(artifact, bundleRaw, producerPin, VerifyOptions{})
}

// VerifyWith is Verify with explicit options.
func VerifyWith(artifact, bundleRaw []byte, producerPin string, opts VerifyOptions) (Verification, error) {
	var out Verification
	if !keyPinPattern.MatchString(producerPin) || len(artifact) > MaxRecordBytes || len(bundleRaw) > core.MaxInputBytes {
		return out, ErrInvalid
	}
	var manifest Manifest
	if core.StrictJSON(artifact, &manifest) != nil || validateManifest(manifest) != nil {
		return out, ErrInvalid
	}
	var bundle core.Bundle
	if core.StrictJSON(bundleRaw, &bundle) != nil || bundle.Delegation != nil || bundle.Acceptance != nil || bundle.Certificate != nil {
		return out, ErrInvalid
	}
	verifyOpts := core.VerifyOptions{}
	if opts.RequirePostQuantum {
		verifyOpts.AcceptedProtocols = []string{core.Protocol03}
	}
	verified, err := core.Verify(bundleRaw, verifyOpts)
	if err != nil || !core.VerifyArtifact(verified, artifact) {
		return out, ErrInvalid
	}
	if verified.Statement.IssuerKeyID != producerPin {
		return out, fmt.Errorf("%w: producer_pin_mismatch", ErrInvalid)
	}
	return Verification{SignatureValid: true, ArtifactMatches: true, ProducerKeyPolicy: "matched", EvidenceScope: Scope, CertificateScope: verified.CertificateScope, IssuerTrust: verified.IssuerTrust, RunID: manifest.RunID, CoreProtocol: verified.Protocol}, nil
}
