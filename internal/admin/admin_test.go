package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

const syntheticImageID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func tempDirectory(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func put(t *testing.T, path string, b []byte) {
	t.Helper()
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func syntheticModel(id string) config.Model {
	return config.Model{ID: id, Revision: strings.Repeat("a", 40), License: "synthetic-test-only", RuntimeImage: "test.invalid/runtime@sha256:" + strings.Repeat("a", 64), Precision: "bfloat16", MaxContext: 2048, MaxTokens: 32, MaxConcurrent: 1}
}
func bundle(t *testing.T, root, id string) config.Model {
	t.Helper()
	source := filepath.Join(root, id+"-source")
	if e := os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	put(t, filepath.Join(source, "config.json"), []byte(`{"model_type":"synthetic"}`))
	put(t, filepath.Join(source, "weights.safetensors"), []byte("synthetic fixture; not actual weights"))
	put(t, filepath.Join(source, "LICENSE"), []byte("synthetic license fixture"))
	put(t, filepath.Join(source, "README.md"), []byte("synthetic model card"))
	image := filepath.Join(root, id+"-image.tar")
	put(t, image, []byte("synthetic archive; never loaded into Docker"))
	m := syntheticModel(id)
	destination := filepath.Join(root, id+"-bundle")
	hash, e := Prepare(source, image, destination, m, syntheticImageID)
	if e != nil {
		t.Fatal(e)
	}
	m.Path = destination
	m.ManifestSHA256 = hash
	return m
}
func fixtureConfig(t *testing.T, root string) (string, config.Config) {
	t.Helper()
	a := bundle(t, root, "model-a")
	b := bundle(t, root, "model-b")
	c := config.Config{Version: 1, Listen: "0.0.0.0:8443", TLSCertFile: "/tls/server.crt", TLSKeyFile: "/tls/server.key", RuntimeURL: "http://runtime:8000", ActiveModel: a.ID, Models: []config.Model{a, b}, Projects: []config.Project{{ID: "project-a", APIKeySHA256: strings.Repeat("3", 64), Models: []string{a.ID, b.ID}, MaxConcurrent: 1}}, TimeoutSeconds: 1}
	p := filepath.Join(root, "config.json")
	raw, _ := json.MarshalIndent(c, "", "  ")
	put(t, p, raw)
	return p, c
}

func TestKeyCreateNoOverwriteAndPermissions(t *testing.T) {
	root := tempDirectory(t)
	p := filepath.Join(root, "project.key")
	h, e := KeyCreate(p)
	if e != nil {
		t.Fatal(e)
	}
	if !validSHA(h) {
		t.Fatal("invalid key hash")
	}
	b, _ := os.ReadFile(p)
	if h != digestBytes(bytes.TrimSuffix(b, []byte("\n"))) {
		t.Fatal("hash includes wrong bytes")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal("key mode must be 0600")
	}
	if _, e = KeyCreate(p); e == nil {
		t.Fatal("overwrote existing secret")
	}
	if got, e := KeyHash(p); e != nil || got != h {
		t.Fatal("hash failed", e)
	}
	if e = os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = KeyHash(p); e == nil {
		t.Fatal("accepted publicly readable key")
	}
	link := filepath.Join(root, "link.key")
	if e = os.Symlink(p, link); e != nil {
		t.Fatal(e)
	}
	if _, e = KeyHash(link); e == nil {
		t.Fatal("followed key symlink")
	}
}

func TestAssetsImportAndRejectTampering(t *testing.T) {
	root := tempDirectory(t)
	m := bundle(t, root, "sample")
	out := filepath.Join(root, "imported")
	if _, e := Import(m.Path, out, m.ManifestSHA256); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Verify(out, m.ManifestSHA256); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"LICENSE", "README.md"} {
		if _, e := os.Stat(filepath.Join(out, "model", name)); e != nil {
			t.Fatal("attribution file was not preserved", e)
		}
	}
	if _, e := Import(m.Path, out, m.ManifestSHA256); e == nil {
		t.Fatal("overwrite accepted")
	}
	put(t, filepath.Join(out, "model/weights.safetensors"), []byte("tampered"))
	if _, _, e := Verify(out, m.ManifestSHA256); e == nil {
		t.Fatal("tampering accepted")
	}
	if _, e := Import(m.Path, filepath.Join(root, "untrusted"), ""); e == nil {
		t.Fatal("untrusted manifest accepted")
	}
}

func TestAssetsRejectPathsLinksAndCode(t *testing.T) {
	for _, name := range []string{"../escape.json", "/absolute.json", "model/../escape.json", "model\\escape.json", "model/code.py", "model/pytorch_model.bin", "model/checkpoint.pkl"} {
		t.Run(name, func(t *testing.T) {
			root := tempDirectory(t)
			m := bundle(t, root, "badpath")
			raw, _ := os.ReadFile(filepath.Join(m.Path, "manifest.json"))
			var manifest Manifest
			json.Unmarshal(raw, &manifest)
			manifest.Files = append(manifest.Files, AssetFile{Path: name, SHA256: strings.Repeat("0", 64), Size: 0})
			raw, _ = json.Marshal(manifest)
			put(t, filepath.Join(m.Path, "manifest.json"), raw)
			if _, _, e := Verify(m.Path, ""); e == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, kind := range []string{"symlink", "unlisted", "remote-code"} {
		t.Run(kind, func(t *testing.T) {
			root := tempDirectory(t)
			m := bundle(t, root, "unsafe")
			switch kind {
			case "symlink":
				if e := os.Symlink(filepath.Join(root, "missing"), filepath.Join(m.Path, "model/link.json")); e != nil {
					t.Fatal(e)
				}
			case "unlisted":
				put(t, filepath.Join(m.Path, "model/extra.json"), []byte("{}"))
			case "remote-code":
				raw, _ := os.ReadFile(filepath.Join(m.Path, "manifest.json"))
				var manifest Manifest
				json.Unmarshal(raw, &manifest)
				payload := []byte(`{"auto_map":{"AutoModel":"evil.Model"}}`)
				put(t, filepath.Join(m.Path, "model/config.json"), payload)
				for i := range manifest.Files {
					if manifest.Files[i].Path == "model/config.json" {
						manifest.Files[i].SHA256 = digestBytes(payload)
						manifest.Files[i].Size = int64(len(payload))
					}
				}
				raw, _ = json.Marshal(manifest)
				put(t, filepath.Join(m.Path, "manifest.json"), raw)
			}
			if _, _, e := Verify(m.Path, ""); e == nil {
				t.Fatal("unsafe asset accepted")
			}
		})
	}
	root := tempDirectory(t)
	source := filepath.Join(root, "src")
	os.Mkdir(source, 0700)
	put(t, filepath.Join(source, "config.json"), []byte("{}"))
	put(t, filepath.Join(source, "model.safetensors"), []byte("fake"))
	put(t, filepath.Join(source, "malicious.pt"), []byte("fake"))
	image := filepath.Join(root, "img.tar")
	put(t, image, []byte("fake"))
	if _, e := Prepare(source, image, filepath.Join(root, "output"), syntheticModel("unsafe"), syntheticImageID); e == nil {
		t.Fatal("pickle preparation accepted")
	}
}

func TestBackupRestoreExcludesSecretFiles(t *testing.T) {
	root := tempDirectory(t)
	p, c := fixtureConfig(t, root)
	secret := filepath.Join(root, "secret.key")
	put(t, secret, []byte("SYNTHETIC-SECRET-MUST-NOT-BE-COPIED"))
	c.TLSKeyFile = secret
	raw, _ := json.Marshal(c)
	put(t, p, raw)
	backup := filepath.Join(root, "backup")
	h, e := Backup(p, backup)
	if e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(backup)
	if e != nil || len(entries) != 2 {
		t.Fatal("unexpected backup contents")
	}
	for _, entry := range entries {
		raw, _ := os.ReadFile(filepath.Join(backup, entry.Name()))
		if bytes.Contains(raw, []byte("SYNTHETIC-SECRET")) {
			t.Fatal("secret leaked into backup")
		}
	}
	restored := filepath.Join(root, "restored.json")
	if e = Restore(backup, h, restored); e != nil {
		t.Fatal(e)
	}
	if _, e = config.Load(restored); e != nil {
		t.Fatal(e)
	}
	if e = Restore(backup, h, restored); e == nil {
		t.Fatal("restore overwrote config")
	}
	put(t, filepath.Join(c.Active().Path, "model/weights.safetensors"), []byte("drift"))
	if e = Restore(backup, h, filepath.Join(root, "other.json")); e == nil {
		t.Fatal("restore accepted asset drift")
	}
}

type fakeExecutor struct {
	commands   []string
	failOnce   string
	failed     bool
	alwaysFail string
}

func (f *fakeExecutor) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s := name + " " + strings.Join(args, " ")
	f.commands = append(f.commands, s)
	if len(args) > 2 && args[0] == "image" && args[1] == "inspect" {
		return []byte(syntheticImageID + "\n"), nil
	}
	if f.alwaysFail != "" && strings.Contains(s, f.alwaysFail) {
		return nil, errors.New("synthetic command failure")
	}
	if !f.failed && f.failOnce != "" && strings.Contains(s, f.failOnce) {
		f.failed = true
		return nil, errors.New("synthetic command failure")
	}
	return nil, nil
}
func activationFixture(t *testing.T) (ActivateOptions, config.Config) {
	t.Helper()
	root := tempDirectory(t)
	p, c := fixtureConfig(t, root)
	compose := filepath.Join(root, "compose.yml")
	put(t, compose, []byte("services: {}\n"))
	env := filepath.Join(root, "deploy.env")
	put(t, env, []byte("APOSTILLE_CONFIG_DIR='"+root+"'\nAPOSTILLE_CONFIG_FILE=config.json\n"))
	state := filepath.Join(root, "state")
	os.Mkdir(state, 0700)
	return ActivateOptions{ConfigPath: p, ModelID: "model-b", ComposeFile: compose, EnvironmentFile: env, StateDirectory: state, ProjectName: "test-local", ReadyTimeout: time.Second}, c
}

func TestActivateDrainsBeforeSwitchAndRestoresPermissions(t *testing.T) {
	o, _ := activationFixture(t)
	os.Chmod(o.ConfigPath, 0640)
	executor := &fakeExecutor{}
	if e := Activate(context.Background(), o, executor, func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
	c, e := config.Load(o.ConfigPath)
	if e != nil || c.ActiveModel != "model-b" {
		t.Fatal("new config not active", e)
	}
	all := strings.Join(executor.commands, "\n")
	stopG := strings.Index(all, "stop --timeout 16 gateway")
	stopR := strings.Index(all, "stop --timeout 30 runtime")
	startR := strings.Index(all, "up -d --no-deps --force-recreate --pull never runtime")
	startG := strings.Index(all, "up -d --no-deps --force-recreate --pull never gateway")
	if stopG < 0 || stopR < stopG || startR < stopR || startG < startR {
		t.Fatalf("unsafe switch order: %s", all)
	}
	st, _ := os.Stat(o.ConfigPath)
	if st.Mode().Perm() != 0640 {
		t.Fatal("config read permissions lost")
	}
	if _, e = os.Stat(filepath.Join(o.StateDirectory, "activation-recovery.json")); !os.IsNotExist(e) {
		t.Fatal("successful operation retained recovery marker")
	}
}

func TestActivateRollsBackStartupFailure(t *testing.T) {
	o, old := activationFixture(t)
	original, _ := os.ReadFile(o.ConfigPath)
	executor := &fakeExecutor{failOnce: "up -d --no-deps --force-recreate --pull never gateway"}
	e := Activate(context.Background(), o, executor, func(context.Context) error { return nil })
	if e == nil || !strings.Contains(e.Error(), "rolled_back") {
		t.Fatal("missing rollback outcome", e)
	}
	restored, _ := os.ReadFile(o.ConfigPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("old config bytes not restored")
	}
	env, _ := os.ReadFile(filepath.Join(o.StateDirectory, "active.env"))
	if !bytes.Contains(env, []byte(old.Active().Path)) {
		t.Fatal("old model environment not restored")
	}
	if strings.Count(strings.Join(executor.commands, "\n"), "up -d --no-deps --force-recreate --pull never runtime") != 2 {
		t.Fatal("old runtime not restarted")
	}
}

func TestActivateRollsBackReadinessFailure(t *testing.T) {
	o, _ := activationFixture(t)
	executor := &fakeExecutor{}
	checks := 0
	e := Activate(context.Background(), o, executor, func(context.Context) error {
		checks++
		c, _ := config.Load(o.ConfigPath)
		if c.ActiveModel == "model-b" {
			return errors.New("new model unhealthy")
		}
		return nil
	})
	if e == nil || e.Error() != "activation_readiness_failed_rolled_back" || checks < 2 {
		t.Fatal("missing readiness rollback", e)
	}
	c, _ := config.Load(o.ConfigPath)
	if c.ActiveModel != "model-a" {
		t.Fatal("old model not restored")
	}
}

func TestActivateKeepsRecoveryWhenRollbackFails(t *testing.T) {
	o, _ := activationFixture(t)
	executor := &fakeExecutor{alwaysFail: "up -d --no-deps --force-recreate --pull never gateway"}
	e := Activate(context.Background(), o, executor, func(context.Context) error { return nil })
	if e == nil || !strings.Contains(e.Error(), "recovery_required") {
		t.Fatal("false rollback success", e)
	}
	if _, e = os.Stat(filepath.Join(o.StateDirectory, "activation-recovery.json")); e != nil {
		t.Fatal("recovery metadata missing")
	}
	second := &fakeExecutor{}
	if e = Activate(context.Background(), o, second, func(context.Context) error { return nil }); e == nil || e.Error() != "unresolved_activation_recovery_exists" {
		t.Fatal("overwrote unresolved recovery", e)
	}
}

func TestReadyCheckRejectsPlaintextAndRemote(t *testing.T) {
	for _, url := range []string{"http://127.0.0.1:8443/readyz", "https://example.com/readyz", "https://localhost/elsewhere", "https://user:password@localhost/readyz"} {
		if _, e := HTTPSReady(url, ""); e == nil {
			t.Fatalf("unsafe readiness URL accepted: %s", url)
		}
	}
}

func TestActivateRefusesDifferentMountedConfig(t *testing.T) {
	o, _ := activationFixture(t)
	put(t, o.EnvironmentFile, []byte("APOSTILLE_CONFIG_DIR=/wrong\n"))
	f := &fakeExecutor{}
	e := Activate(context.Background(), o, f, func(context.Context) error { return nil })
	if e == nil || e.Error() != "deployment_config_mount_mismatch" || len(f.commands) != 0 {
		t.Fatal("wrong config mount not caught before Docker", e)
	}
}

func TestImmutableImageLookupAndMismatch(t *testing.T) {
	f := &fakeExecutor{}
	if _, e := ResolveImageID(context.Background(), f, "runtime:latest"); e == nil {
		t.Fatal("mutable image tag accepted")
	}
	id, e := ResolveImageID(context.Background(), f, "registry.invalid/runtime@sha256:"+strings.Repeat("a", 64))
	if e != nil || id != syntheticImageID {
		t.Fatal("image mapping failed", e)
	}
	if e = CheckImage(context.Background(), f, "sha256:"+strings.Repeat("c", 64)); e == nil {
		t.Fatal("wrong loaded image accepted")
	}
}

func TestOpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	root := tempDirectory(t)
	fifo := filepath.Join(root, "weights.safetensors")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	r, e := os.OpenRoot(root)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	done := make(chan error, 1)
	go func() {
		f, e := openRegular(r, "weights.safetensors")
		if f != nil {
			f.Close()
		}
		done <- e
	}()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked asset validation")
	}
}

func TestExecutorDoesNotInheritComposeOverrides(t *testing.T) {
	t.Setenv("APOSTILLE_RUNTIME_IMAGE", "synthetic-wrong-image")
	t.Setenv("APOSTILLE_MODEL_BUNDLE", "/synthetic-wrong-model")
	t.Setenv("COMPOSE_FILE", "synthetic-wrong-compose")
	t.Setenv("APOSTILLE_HELPER_TEST", "must-be-removed")
	t.Setenv("GO_ADMIN_HELPER", "1")
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	b, e := (OSExecutor{}).Run(context.Background(), binary, "-test.run=TestAdminExecutorHelper")
	if e != nil {
		t.Fatal(e)
	}
	if string(b) != "clean\n" {
		t.Fatalf("environment override leaked: %s", b)
	}
}
func TestAdminExecutorHelper(t *testing.T) {
	if os.Getenv("GO_ADMIN_HELPER") != "1" {
		return
	}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "APOSTILLE_") || strings.HasPrefix(entry, "COMPOSE_") {
			os.Exit(2)
		}
	}
	os.Stdout.WriteString("clean\n")
	os.Exit(0)
}
func TestExecutorRejectsRemoteDockerHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://example.invalid:2375")
	if _, e := (OSExecutor{}).Run(context.Background(), "docker", "info"); e == nil || e.Error() != "local_unix_docker_socket_required" {
		t.Fatal("remote Docker context accepted", e)
	}
}

type inventoryExecutor map[string]string

func (f inventoryExecutor) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s, ok := f[name+" "+strings.Join(args, " ")]
	if !ok {
		return nil, errors.New("missing synthetic tool")
	}
	return []byte(s), nil
}
func TestDoctorReportsMissingAndUnverifiedHardware(t *testing.T) {
	ex := inventoryExecutor{"docker version --format {{.Server.Version}}": "28.0.0", "docker compose version --short": "2.31.0", "nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader,nounits": "Synthetic GPU, 999.0, 24000"}
	r, e := DoctorWith(context.Background(), "", ex, func() ([]byte, error) { return []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\n"), nil }, "linux")
	if e != nil {
		t.Fatal(e)
	}
	if r.Hardware != "unverified" {
		t.Fatal("inventory claimed hardware validation")
	}
	statuses := map[string]string{}
	for _, c := range r.Checks {
		statuses[c.Component] = c.Status
		if c.Guidance == "" {
			t.Fatal("missing corrective guidance")
		}
	}
	if statuses["nvidia_gpu"] != "unverified" || statuses["amd_gpu"] != "not_applicable" || statuses["configuration"] != "missing" {
		t.Fatal("incorrect inventory state", statuses)
	}
	r, e = DoctorWith(context.Background(), "", inventoryExecutor{}, func() ([]byte, error) { return nil, errors.New("missing") }, "linux")
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range r.Checks {
		if c.Status != "missing" {
			t.Fatal("missing tools falsely accepted", c)
		}
	}
}
