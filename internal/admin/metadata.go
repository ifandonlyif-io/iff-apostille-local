package admin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

func KeyCreate(destination string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := "apl_" + base64.RawURLEncoding.EncodeToString(b)
	if err := writeExclusive(destination, []byte(key+"\n")); err != nil {
		return "", err
	}
	return digestBytes([]byte(key)), nil
}
func KeyHash(file string) (string, error) {
	b, err := readSmall(file, 4096)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0077 != 0 {
		return "", errors.New("key_file_must_be_owner_only")
	}
	key := strings.TrimSuffix(string(b), "\n")
	if len(key) < 32 || strings.ContainsAny(key, "\r\n\t ") {
		return "", errors.New("invalid_api_key")
	}
	return digestBytes([]byte(key)), nil
}

type backupManifest struct {
	Version      int      `json:"version"`
	Kind         string   `json:"kind"`
	ConfigSHA256 string   `json:"config_sha256"`
	Excluded     []string `json:"excluded"`
}

// Backup includes only validated configuration metadata. Private key files,
// prompts, results, model weights, caches and image archives are never read.
func Backup(configPath, destination string) (string, error) {
	c, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err = noSymlinkPath(filepath.Dir(destination)); err != nil {
		return "", err
	}
	if _, e := os.Lstat(destination); !os.IsNotExist(e) {
		return "", errors.New("destination_must_not_exist")
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), ".apostille-backup-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err = writeExclusive(filepath.Join(tmp, "config.json"), b); err != nil {
		return "", err
	}
	m := backupManifest{Version: 1, Kind: "configuration-metadata-only", ConfigSHA256: digestBytes(b), Excluded: []string{"API key values", "TLS private keys", "Apostille signing keys", "prompts and outputs", "evidence artifacts", "models and image archives"}}
	mb, _ := json.MarshalIndent(m, "", "  ")
	mb = append(mb, '\n')
	if err = writeExclusive(filepath.Join(tmp, "backup.json"), mb); err != nil {
		return "", err
	}
	if err = os.Rename(tmp, destination); err != nil {
		return "", err
	}
	return digestBytes(mb), nil
}

// Restore creates a new config file. Existing installations are never overwritten.
// Import model bundles and independently provision secret files before restoring.
func Restore(backupDirectory, expectedHash, destination string) error {
	if !validSHA(expectedHash) {
		return errors.New("trusted_backup_hash_required")
	}
	if err := noSymlinkPath(backupDirectory); err != nil {
		return err
	}
	entries, err := os.ReadDir(backupDirectory)
	if err != nil {
		return err
	}
	if len(entries) != 2 {
		return errors.New("unexpected_backup_files")
	}
	mb, err := readSmall(filepath.Join(backupDirectory, "backup.json"), 1<<20)
	if err != nil {
		return err
	}
	if digestBytes(mb) != expectedHash {
		return errors.New("backup_hash_mismatch")
	}
	var m backupManifest
	if err = decodeStrict(mb, &m); err != nil {
		return err
	}
	if m.Version != 1 || m.Kind != "configuration-metadata-only" || !validSHA(m.ConfigSHA256) {
		return errors.New("invalid_backup_metadata")
	}
	b, err := readSmall(filepath.Join(backupDirectory, "config.json"), 1<<20)
	if err != nil {
		return err
	}
	if digestBytes(b) != m.ConfigSHA256 {
		return errors.New("backup_config_hash_mismatch")
	}
	var c config.Config
	if err = decodeStrict(b, &c); err != nil {
		return err
	}
	if err = c.Validate(); err != nil {
		return err
	}
	for _, model := range c.Models {
		manifest, hash, e := Verify(model.Path, model.ManifestSHA256)
		if e != nil {
			return errors.New("restore_model_assets_unavailable_or_invalid")
		}
		if e = MatchModel(model, manifest, hash); e != nil {
			return e
		}
	}
	for _, p := range []string{c.TLSCertFile, c.TLSKeyFile, c.Evidence.KeyFile} {
		if p != "" && !filepath.IsAbs(p) {
			return errors.New("restore_secret_references_must_be_absolute")
		}
	}
	return writeExclusive(destination, b)
}
