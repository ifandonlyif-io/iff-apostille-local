package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

const manifestLimit = 4 << 20
const maxAssetFiles = 100000

type AssetFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	Version        int          `json:"version"`
	Model          config.Model `json:"model"`
	RuntimeImageID string       `json:"runtime_image_id"`
	Files          []AssetFile  `json:"files"`
}

func decodeStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid_metadata")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("invalid_metadata")
	}
	return nil
}
func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validateModelMetadata(m config.Model) error {
	// Path and ManifestSHA256 are installation data and are never self-referenced.
	if m.Path != "" || m.ManifestSHA256 != "" {
		return errors.New("manifest_model_contains_installation_fields")
	}
	c := config.Config{Version: 1, Listen: "127.0.0.1:8443", RuntimeURL: "http://runtime:8000", ActiveModel: m.ID, TimeoutSeconds: 60, Models: []config.Model{m}, Projects: []config.Project{{ID: "validation", APIKeySHA256: strings.Repeat("1", 64), Models: []string{m.ID}, MaxConcurrent: 1}}}
	c.Models[0].Path = "/validation"
	c.Models[0].ManifestSHA256 = strings.Repeat("1", 64)
	if err := c.Validate(); err != nil {
		return errors.New("invalid_model_metadata")
	}
	for _, s := range []string{m.ID, m.RuntimeImage, m.Revision, m.License} {
		if strings.ContainsAny(s, "\x00\r\n") {
			return errors.New("invalid_model_metadata")
		}
	}
	if strings.ContainsAny(m.RuntimeImage, " \t'\"$`\\") {
		return errors.New("invalid_runtime_image")
	}
	return nil
}

func allowedModelFile(name string) bool {
	if !cleanRelative(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	switch strings.ToUpper(path.Base(name)) {
	case "LICENSE", "LICENCE", "NOTICE", "COPYING":
		return true
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".safetensors", ".model", ".txt", ".md", ".tiktoken", ".vocab", ".merges":
		return true
	}
	return false
}

// Prepare copies only explicitly permitted local model files. It never fetches a
// repository or imports Python code. The caller must supply the pinned OCI image.
func Prepare(source, imageArchive, destination string, model config.Model, imageID string) (string, error) {
	if !validImageID(imageID) {
		return "", errors.New("immutable_image_id_required")
	}
	if err := validateModelMetadata(model); err != nil {
		return "", err
	}
	if err := noSymlinkPath(source); err != nil {
		return "", err
	}
	if err := noSymlinkPath(imageArchive); err != nil {
		return "", err
	}
	if err := noSymlinkPath(filepath.Dir(destination)); err != nil {
		return "", err
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return "", errors.New("destination_must_not_exist")
	}
	// Refuse nesting the output inside its source (including source == parent).
	srcAbs, _ := filepath.Abs(source)
	dstAbs, _ := filepath.Abs(destination)
	if dstAbs == srcAbs || strings.HasPrefix(dstAbs, srcAbs+string(filepath.Separator)) {
		return "", errors.New("destination_inside_source")
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return "", err
	}
	defer sourceRoot.Close()
	names := []string{}
	hasWeights := false
	hasConfig := false
	err = fs.WalkDir(sourceRoot.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlinks_not_allowed")
		}
		if d.IsDir() {
			return nil
		}
		if !allowedModelFile(name) {
			return fmt.Errorf("unsupported_model_file: %s", name)
		}
		if strings.HasSuffix(strings.ToLower(name), ".safetensors") {
			hasWeights = true
		}
		if name == "config.json" {
			hasConfig = true
		}
		names = append(names, name)
		if len(names) > maxAssetFiles {
			return errors.New("too_many_files")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !hasWeights || !hasConfig {
		return "", errors.New("safetensors_and_config_required")
	}
	sort.Strings(names)
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".apostille-assets-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	manifest := Manifest{Version: 1, Model: model, RuntimeImageID: imageID}
	for _, name := range names {
		h, n, err := copyFile(sourceRoot, name, filepath.Join(staging, "model", filepath.FromSlash(name)))
		if err != nil {
			return "", err
		}
		manifest.Files = append(manifest.Files, AssetFile{Path: "model/" + name, SHA256: h, Size: n})
	}
	imageRoot, err := os.OpenRoot(filepath.Dir(imageArchive))
	if err != nil {
		return "", err
	}
	defer imageRoot.Close()
	h, n, err := copyFile(imageRoot, filepath.Base(imageArchive), filepath.Join(staging, "image/runtime.tar"))
	if err != nil {
		return "", err
	}
	manifest.Files = append(manifest.Files, AssetFile{Path: "image/runtime.tar", SHA256: h, Size: n})
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err = writeExclusive(filepath.Join(staging, "manifest.json"), b); err != nil {
		return "", err
	}
	if _, _, err = Verify(staging, digestBytes(b)); err != nil {
		return "", err
	}
	if err = os.Rename(staging, destination); err != nil {
		return "", err
	}
	return digestBytes(b), nil
}

func ReadManifest(directory, expectedHash string) (Manifest, string, error) {
	var m Manifest
	if err := noSymlinkPath(directory); err != nil {
		return m, "", err
	}
	b, err := readSmall(filepath.Join(directory, "manifest.json"), manifestLimit)
	if err != nil {
		return m, "", err
	}
	h := digestBytes(b)
	if expectedHash != "" && (!validSHA(expectedHash) || h != expectedHash) {
		return m, "", errors.New("manifest_hash_mismatch")
	}
	if err = decodeStrict(b, &m); err != nil {
		return m, "", err
	}
	if m.Version != 1 || !validImageID(m.RuntimeImageID) || len(m.Files) == 0 || len(m.Files) > maxAssetFiles+1 {
		return m, "", errors.New("invalid_manifest")
	}
	if err = validateModelMetadata(m.Model); err != nil {
		return m, "", err
	}
	seen := map[string]bool{}
	hasImage := false
	hasWeights := false
	hasConfig := false
	for _, f := range m.Files {
		if !cleanRelative(f.Path) || seen[f.Path] || !validSHA(f.SHA256) || f.Size < 0 {
			return m, "", errors.New("invalid_manifest_file")
		}
		seen[f.Path] = true
		if f.Path == "image/runtime.tar" {
			hasImage = true
			continue
		}
		if !strings.HasPrefix(f.Path, "model/") || !allowedModelFile(strings.TrimPrefix(f.Path, "model/")) {
			return m, "", errors.New("unsupported_model_file")
		}
		if strings.HasSuffix(strings.ToLower(f.Path), ".safetensors") {
			hasWeights = true
		}
		if f.Path == "model/config.json" {
			hasConfig = true
		}
	}
	if !hasImage || !hasWeights || !hasConfig {
		return m, "", errors.New("incomplete_manifest")
	}
	return m, h, nil
}

func Verify(directory, expectedHash string) (Manifest, string, error) {
	m, h, err := ReadManifest(directory, expectedHash)
	if err != nil {
		return m, "", err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return m, "", err
	}
	defer root.Close()
	expected := map[string]bool{"manifest.json": true}
	for _, entry := range m.Files {
		expected[entry.Path] = true
		f, err := openRegular(root, entry.Path)
		if err != nil {
			return m, "", err
		}
		got, size, err := digestReader(f)
		f.Close()
		if err != nil {
			return m, "", err
		}
		if size != entry.Size || got != entry.SHA256 {
			return m, "", errors.New("asset_hash_mismatch")
		}
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if name == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlinks_not_allowed")
		}
		if !d.IsDir() && !expected[name] {
			return errors.New("unmanifested_asset")
		}
		return nil
	})
	if err != nil {
		return m, "", err
	}
	// A model requiring custom Python code cannot be made safe by file filtering.
	b, err := readSmall(filepath.Join(directory, "model/config.json"), manifestLimit)
	if err != nil {
		return m, "", err
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return m, "", errors.New("invalid_model_config")
	}
	if _, ok := obj["auto_map"]; ok {
		return m, "", errors.New("remote_model_code_not_supported")
	}
	return m, h, nil
}

func MatchModel(installed config.Model, manifest Manifest, hash string) error {
	if installed.ManifestSHA256 != hash {
		return errors.New("manifest_hash_mismatch")
	}
	installed.Path = ""
	installed.ManifestSHA256 = ""
	if installed != manifest.Model {
		return errors.New("model_metadata_mismatch")
	}
	return nil
}

// Import verifies before copying and then verifies the copied bytes again. It
// never extracts an archive; Docker is the only consumer of the OCI image tar.
func Import(source, destination, expectedHash string) (Manifest, error) {
	var zero Manifest
	if !validSHA(expectedHash) {
		return zero, errors.New("trusted_manifest_hash_required")
	}
	m, _, err := Verify(source, expectedHash)
	if err != nil {
		return zero, err
	}
	if err = noSymlinkPath(filepath.Dir(destination)); err != nil {
		return zero, err
	}
	if _, e := os.Lstat(destination); !os.IsNotExist(e) {
		return zero, errors.New("destination_must_not_exist")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return zero, err
	}
	defer root.Close()
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".apostille-import-*")
	if err != nil {
		return zero, err
	}
	defer os.RemoveAll(staging)
	for _, entry := range m.Files {
		if _, _, err = copyFile(root, entry.Path, filepath.Join(staging, filepath.FromSlash(entry.Path))); err != nil {
			return zero, err
		}
	}
	if _, _, err = copyFile(root, "manifest.json", filepath.Join(staging, "manifest.json")); err != nil {
		return zero, err
	}
	if _, _, err = Verify(staging, expectedHash); err != nil {
		return zero, err
	}
	if err = os.Rename(staging, destination); err != nil {
		return zero, err
	}
	return m, nil
}

func LoadImage(ctx context.Context, executor Executor, bundle string, manifest Manifest) error {
	if _, err := executor.Run(ctx, "docker", "image", "load", "--input", filepath.Join(bundle, "image/runtime.tar")); err != nil {
		return errors.New("image_load_failed")
	}
	return CheckImage(ctx, executor, manifest.RuntimeImageID)
}
func validImageID(id string) bool {
	return strings.HasPrefix(id, "sha256:") && validSHA(strings.TrimPrefix(id, "sha256:"))
}

// ResolveImageID runs only during staging. Docker resolves the immutable upstream
// manifest digest; recording its config ID survives Docker save/load losing tags.
func ResolveImageID(ctx context.Context, executor Executor, pinned string) (string, error) {
	parts := strings.Split(pinned, "@sha256:")
	if len(parts) != 2 || parts[0] == "" || !validSHA(parts[1]) {
		return "", errors.New("repository_digest_required")
	}
	out, err := executor.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", pinned)
	if err != nil {
		return "", errors.New("pinned_image_not_available")
	}
	id := strings.TrimSpace(string(out))
	if !validImageID(id) {
		return "", errors.New("image_inspection_invalid")
	}
	return id, nil
}
func CheckImage(ctx context.Context, executor Executor, imageID string) error {
	if !validImageID(imageID) {
		return errors.New("immutable_image_id_required")
	}
	out, err := executor.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", imageID)
	if err != nil {
		return errors.New("pinned_image_not_available")
	}
	if strings.TrimSpace(string(out)) == imageID {
		return nil
	}
	return errors.New("image_digest_not_loaded")
}
