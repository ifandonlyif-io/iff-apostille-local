package admin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

type Executor interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type OSExecutor struct{}
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 65536 {
		keep := 65536 - b.Len()
		if keep > len(p) {
			keep = len(p)
		}
		_, _ = b.Buffer.Write(p[:keep])
	}
	return n, nil
}
func (OSExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = commandEnvironment(os.Environ())
	if name == "docker" {
		host := ""
		for _, entry := range cmd.Env {
			if strings.HasPrefix(entry, "DOCKER_HOST=") {
				host = strings.TrimPrefix(entry, "DOCKER_HOST=")
			}
		}
		if host == "" {
			inspect := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
			inspect.Env = cmd.Env
			var data cappedBuffer
			inspect.Stdout = &data
			if inspect.Run() != nil {
				return nil, errors.New("local_docker_context_unavailable")
			}
			host = strings.TrimSpace(data.String())
		}
		if !strings.HasPrefix(host, "unix:///") || strings.ContainsAny(host, "\r\n\x00") {
			return nil, errors.New("local_unix_docker_socket_required")
		}
		env := []string{}
		for _, entry := range cmd.Env {
			key := strings.SplitN(entry, "=", 2)[0]
			if key != "DOCKER_HOST" && key != "DOCKER_CONTEXT" && key != "DOCKER_TLS_VERIFY" && key != "DOCKER_CERT_PATH" {
				env = append(env, entry)
			}
		}
		cmd.Env = append(env, "DOCKER_HOST="+host)
	}
	var out cappedBuffer
	cmd.Stdout = &out
	// Docker/runtime stderr may contain environment or input metadata. Do not emit it.
	if err := cmd.Run(); err != nil {
		return nil, errors.New("local_command_failed")
	}
	return out.Bytes(), nil
}

func commandEnvironment(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		key := strings.SplitN(item, "=", 2)[0]
		if strings.HasPrefix(key, "APOSTILLE_") || strings.HasPrefix(key, "COMPOSE_") {
			continue
		}
		out = append(out, item)
	}
	return out
}

type ActivateOptions struct {
	ConfigPath, ModelID, ComposeFile, EnvironmentFile, StateDirectory, ProjectName string
	ReadyURL, CAFile                                                               string
	ReadyTimeout                                                                   time.Duration
}
type ReadyFunc func(context.Context) error

func HTTPSReady(rawURL, caFile string) (ReadyFunc, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/readyz" {
		return nil, errors.New("https_readyz_url_required")
	}
	// The operator runs this check locally; never contact an arbitrary remote URL.
	if u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return nil, errors.New("readiness_must_use_loopback")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		b, e := readSmall(caFile, 1<<20)
		if e != nil {
			return nil, e
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid_ca_file")
		}
		tlsConfig.RootCAs = pool
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context) error {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if e != nil {
			return e
		}
		r, e := client.Do(req)
		if e != nil {
			return errors.New("gateway_not_ready")
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			return errors.New("gateway_not_ready")
		}
		return nil
	}, nil
}

func activeEnvironment(c config.Config) ([]byte, error) {
	m := c.Active()
	if !filepath.IsAbs(m.Path) {
		return nil, errors.New("model_bundle_path_must_be_absolute")
	}
	manifest, _, err := ReadManifest(m.Path, m.ManifestSHA256)
	if err != nil {
		return nil, err
	}
	vars := [][2]string{{"APOSTILLE_RUNTIME_IMAGE", manifest.RuntimeImageID}, {"APOSTILLE_MODEL_BUNDLE", m.Path}, {"APOSTILLE_ACTIVE_MODEL", m.ID}}
	var b strings.Builder
	for _, kv := range vars {
		if strings.ContainsAny(kv[1], "\x00\r\n'\\") {
			return nil, errors.New("unsafe_environment_value")
		}
		fmt.Fprintf(&b, "%s='%s'\n", kv[0], kv[1])
	}
	return []byte(b.String()), nil
}

func lockState(directory string) (func(), error) {
	if err := noSymlinkPath(directory); err != nil {
		return nil, err
	}
	f, e := os.OpenFile(filepath.Join(directory, "admin.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another_management_operation_is_running")
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func Activate(ctx context.Context, o ActivateOptions, executor Executor, ready ReadyFunc) error {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`).MatchString(o.ProjectName) || !config.ValidID(o.ModelID) || o.ReadyTimeout < time.Second {
		return errors.New("invalid_activation_options")
	}
	for _, p := range []string{o.ComposeFile, o.EnvironmentFile, o.ConfigPath, o.StateDirectory} {
		if !filepath.IsAbs(p) {
			return errors.New("activation_paths_must_be_absolute")
		}
		if err := noSymlinkPath(p); err != nil {
			return err
		}
	}
	if err := checkConfigMount(o.EnvironmentFile, o.ConfigPath); err != nil {
		return err
	}
	unlock, err := lockState(o.StateDirectory)
	if err != nil {
		return err
	}
	defer unlock()
	original, err := readSmall(o.ConfigPath, 1<<20)
	if err != nil {
		return err
	}
	old, err := config.Load(o.ConfigPath)
	if err != nil {
		return err
	}
	next := old
	next.ActiveModel = o.ModelID
	if err = next.Validate(); err != nil {
		return errors.New("unknown_model")
	}
	for _, m := range []config.Model{old.Active(), next.Active()} {
		manifest, hash, e := Verify(m.Path, m.ManifestSHA256)
		if e != nil {
			return e
		}
		if e = MatchModel(m, manifest, hash); e != nil {
			return e
		}
		if e = CheckImage(ctx, executor, manifest.RuntimeImageID); e != nil {
			return e
		}
	}
	oldEnv, err := activeEnvironment(old)
	if err != nil {
		return err
	}
	newEnv, err := activeEnvironment(next)
	if err != nil {
		return err
	}
	envPath := filepath.Join(o.StateDirectory, "active.env")
	if existing, e := readSmall(envPath, 1<<20); e == nil {
		if !bytes.Equal(existing, oldEnv) {
			return errors.New("active_environment_config_mismatch")
		}
	} else if os.IsNotExist(e) {
		if e = writeExclusive(envPath, oldEnv); e != nil {
			return e
		}
	} else {
		return e
	}
	// Persist recovery metadata before stopping anything. This is not a secret backup.
	recovery := filepath.Join(o.StateDirectory, "activation-recovery.json")
	if err = writeExclusive(recovery, original); err != nil {
		return errors.New("unresolved_activation_recovery_exists")
	}
	cleanRecovery := false
	defer func() {
		if cleanRecovery {
			os.Remove(recovery)
			syncDirectory(o.StateDirectory)
		}
	}()
	base := []string{"compose", "--project-name", o.ProjectName, "--env-file", o.EnvironmentFile, "--env-file", envPath, "-f", o.ComposeFile}
	run := func(c context.Context, args ...string) error {
		_, e := executor.Run(c, "docker", append(append([]string{}, base...), args...)...)
		return e
	}
	stop := func(c context.Context) error {
		if e := run(c, "stop", "--timeout", strconv.Itoa(old.TimeoutSeconds+15), "gateway"); e != nil {
			return e
		}
		return run(c, "stop", "--timeout", "30", "runtime")
	}
	start := func(c context.Context) error {
		if e := run(c, "up", "-d", "--no-deps", "--force-recreate", "--pull", "never", "runtime"); e != nil {
			return e
		}
		return run(c, "up", "-d", "--no-deps", "--force-recreate", "--pull", "never", "gateway")
	}
	check := func(c context.Context) error {
		c, cancel := context.WithTimeout(c, o.ReadyTimeout)
		defer cancel()
		for {
			if e := ready(c); e == nil {
				return nil
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-c.Done():
				timer.Stop()
				return errors.New("readiness_timeout")
			case <-timer.C:
			}
		}
	}
	rollback := func() error {
		// A canceled caller must not prevent restoration. Recovery has its own bound.
		c, cancel := context.WithTimeout(context.Background(), o.ReadyTimeout+time.Duration(old.TimeoutSeconds+120)*time.Second)
		defer cancel()
		stopErr := stop(c)
		if e := replaceFile(o.ConfigPath, original); e != nil {
			return errors.New("rollback_config_failed_recovery_required")
		}
		if e := replaceFile(envPath, oldEnv); e != nil {
			return errors.New("rollback_environment_failed_recovery_required")
		}
		if stopErr != nil {
			return errors.New("rollback_stop_failed_recovery_required")
		}
		if e := start(c); e != nil {
			return errors.New("rollback_start_failed_recovery_required")
		}
		if e := check(c); e != nil {
			return errors.New("rollback_readiness_failed_recovery_required")
		}
		cleanRecovery = true
		return nil
	}
	fail := func(reason string) error {
		if e := rollback(); e != nil {
			return e
		}
		return errors.New(reason + "_rolled_back")
	}
	if err = stop(ctx); err != nil {
		return fail("activation_stop_failed")
	}
	nextBytes, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fail("activation_config_failed")
	}
	nextBytes = append(nextBytes, '\n')
	if err = replaceFile(o.ConfigPath, nextBytes); err != nil {
		return fail("activation_config_failed")
	}
	if err = replaceFile(envPath, newEnv); err != nil {
		return fail("activation_environment_failed")
	}
	if err = start(ctx); err != nil {
		return fail("activation_start_failed")
	}
	if err = check(ctx); err != nil {
		return fail("activation_readiness_failed")
	}
	cleanRecovery = true
	return nil
}

func checkConfigMount(environmentFile, configFile string) error {
	b, e := readSmall(environmentFile, 1<<20)
	if e != nil {
		return e
	}
	values := map[string]string{"APOSTILLE_CONFIG_FILE": "config.json"}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		kv := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		if key != "APOSTILLE_CONFIG_DIR" && key != "APOSTILLE_CONFIG_FILE" {
			continue
		}
		if seen[key] {
			return errors.New("duplicate_config_mount_setting")
		}
		seen[key] = true
		value := strings.TrimSpace(kv[1])
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		if strings.ContainsAny(value, "$`\r\n\\") {
			return errors.New("config_mount_must_be_literal")
		}
		values[key] = value
	}
	if values["APOSTILLE_CONFIG_DIR"] != filepath.Dir(configFile) || values["APOSTILLE_CONFIG_FILE"] != filepath.Base(configFile) {
		return errors.New("deployment_config_mount_mismatch")
	}
	return nil
}
