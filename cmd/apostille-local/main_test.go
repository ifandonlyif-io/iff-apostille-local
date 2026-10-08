package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
)

const processToken = "synthetic-process-test-project-token"

// Re-exec the test binary so main owns real signals, TLS listeners and shutdown.
// No production behavior is replaced by a mock inside the gateway process.
func TestGatewayHelperProcess(t *testing.T) {
	if os.Getenv("APOSTILLE_LOCAL_PROCESS_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("apostille-local", flag.ContinueOnError)
	main()
	os.Exit(0)
}

type gatewayProcess struct {
	cmd    *exec.Cmd
	output bytes.Buffer
	done   chan struct{}
	err    error
}

func startGatewayProcess(t *testing.T, path string, args ...string) *gatewayProcess {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"-test.run=^TestGatewayHelperProcess$", "--", "--config", path}, args...)
	p := &gatewayProcess{cmd: exec.Command(binary, argv...), done: make(chan struct{})}
	p.cmd.Env = append(os.Environ(), "APOSTILLE_LOCAL_PROCESS_TEST=1")
	p.cmd.Stdout, p.cmd.Stderr = &p.output, &p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Error("gateway helper did not exit after kill")
			}
		}
	})
	return p
}

func (p *gatewayProcess) wait(t *testing.T, wantSuccess bool) {
	t.Helper()
	select {
	case <-p.done:
		if (p.err == nil) != wantSuccess {
			t.Fatalf("gateway exit: %v; output=%s", p.err, p.output.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("gateway process did not exit")
	}
}

type processFixture struct {
	config config.Config
	path   string
	client *http.Client
	url    string
}

func newProcessFixture(t *testing.T, timeout int, handler http.HandlerFunc) processFixture {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"synthetic-model"}]}`)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(backend.Close)
	dir := t.TempDir()
	certPath, keyPath, roots := processCertificate(t, dir)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	seed := filepath.Join(dir, "seed")
	if err = os.WriteFile(seed, bytes.Repeat([]byte{31}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(processToken))
	model := config.Model{ID: "synthetic-model", Revision: strings.Repeat("a", 40), ManifestSHA256: strings.Repeat("b", 64), RuntimeImage: "example/runtime@sha256:" + strings.Repeat("c", 64), License: "synthetic", Precision: "float16", MaxContext: 1024, MaxTokens: 128, MaxConcurrent: 1, Path: "/synthetic/model"}
	c := config.Config{Version: 1, Listen: address, TLSCertFile: certPath, TLSKeyFile: keyPath, RuntimeURL: backend.URL, ActiveModel: model.ID, Models: []config.Model{model}, Projects: []config.Project{{ID: "project", APIKeySHA256: hex.EncodeToString(hash[:]), Models: []string{model.ID}, MaxConcurrent: 1}}, Evidence: config.Evidence{Directory: filepath.Join(dir, "records"), KeyFile: seed, AgentID: "11111111-1111-4111-8111-111111111111"}, TimeoutSeconds: timeout}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, DisableKeepAlives: true, Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	return processFixture{config: c, path: filepath.Join(dir, "config.json"), client: &http.Client{Transport: transport, Timeout: 4 * time.Second}, url: "https://" + address}
}

func processCertificate(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-local-test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for name, content := range map[string][]byte{certPath: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPath: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})} {
		if err := os.WriteFile(name, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return certPath, keyPath, roots
}

func (f processFixture) start(t *testing.T, args ...string) *gatewayProcess {
	t.Helper()
	b, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return startGatewayProcess(t, f.path, args...)
}

func (f processFixture) ready(t *testing.T, p *gatewayProcess) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			t.Fatalf("gateway exited before readiness: %v %s", p.err, p.output.String())
		default:
		}
		response, err := f.client.Get(f.url + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateway TLS readiness deadline exceeded")
}

func (f processFixture) inference(ctx context.Context, stream bool) (*http.Response, error) {
	body := fmt.Sprintf(`{"model":"synthetic-model","messages":[{"role":"user","content":"synthetic-process-input"}],"stream":%t}`, stream)
	r, err := http.NewRequestWithContext(ctx, "POST", f.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+processToken)
	r.Header.Set("X-Apostille-Record", "metadata")
	return f.client.Do(r)
}

func TestProcessRequiresTLSAndRestrictsInsecureMode(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		t.Run(fmt.Sprintf("insecure=%t", insecure), func(t *testing.T) {
			f := newProcessFixture(t, 1, func(http.ResponseWriter, *http.Request) { t.Error("invalid startup reached runtime") })
			f.config.TLSCertFile, f.config.TLSKeyFile = "", ""
			var args []string
			if insecure {
				f.config.Listen = "0.0.0.0:0"
				args = []string{"--insecure-test"}
			}
			p := f.start(t, args...)
			p.wait(t, false)
			if strings.TrimSpace(p.output.String()) != "apostille_local_start_failed" {
				t.Fatal("startup failure was not sanitized")
			}
		})
	}
}

func TestProcessSIGTERMDrainsAdmittedRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newProcessFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-release:
			fmt.Fprint(w, `{"model":"synthetic-model","choices":[{"index":0,"message":{"role":"assistant","content":"synthetic-process-output"},"finish_reason":"stop"}]}`)
		case <-r.Context().Done():
		}
	})
	p := f.start(t)
	f.ready(t, p)
	type answer struct {
		status int
		runID  string
		body   []byte
		err    error
	}
	result := make(chan answer, 1)
	go func() {
		response, err := f.inference(context.Background(), false)
		if err != nil {
			result <- answer{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		result <- answer{status: response.StatusCode, runID: response.Header.Get("X-Apostille-Run-ID"), body: body, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach runtime")
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Wait for Shutdown to close the listener while the admitted request remains.
	closed := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", f.config.Listen, 50*time.Millisecond)
		if err != nil {
			closed = true
			break
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	if !closed {
		t.Fatal("SIGTERM did not stop accepting new connections")
	}
	select {
	case <-p.done:
		t.Fatal("gateway exited before draining admitted request")
	default:
	}
	close(release)
	var got answer
	select {
	case got = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("admitted request did not finish during drain")
	}
	if got.err != nil || got.status != 200 || !bytes.Contains(got.body, []byte("synthetic-process-output")) {
		t.Fatalf("drained inference failed: status=%d error=%v", got.status, got.err)
	}
	p.wait(t, true)
	store, err := evidence.New(f.config.Evidence.Directory, f.config.Evidence.KeyFile, f.config.Evidence.AgentID)
	if err != nil {
		t.Fatal("shutdown did not release evidence owner lock", err)
	}
	defer store.Close()
	if record, err := store.Get("project", got.runID); err != nil || record.Status != "ready" {
		t.Fatal("drained completion receipt was not durable", err)
	}
}

func TestProcessTimeoutAndClientCancellationCloseBackend(t *testing.T) {
	canceled := make(chan struct{}, 2)
	f := newProcessFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Stream bool }
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"model\":\"synthetic-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"synthetic\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
		canceled <- struct{}{}
	})
	p := f.start(t)
	f.ready(t, p)
	response, err := f.inference(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("timeout returned %d", response.StatusCode)
	}
	awaitBackendCancellation(t, canceled)
	waitFailedReceipt(t, f, response.Header.Get("X-Apostille-Run-ID"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err = f.inference(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		t.Fatalf("stream not admitted after timeout: %d", response.StatusCode)
	}
	cancel()
	response.Body.Close()
	awaitBackendCancellation(t, canceled)
	waitFailedReceipt(t, f, response.Header.Get("X-Apostille-Run-ID"))
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	p.wait(t, true)
}

func awaitBackendCancellation(t *testing.T, canceled <-chan struct{}) {
	t.Helper()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not cancel runtime request")
	}
}

func waitFailedReceipt(t *testing.T, f processFixture, runID string) {
	t.Helper()
	if runID == "" {
		t.Fatal("missing run ID")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r, _ := http.NewRequest("GET", f.url+"/local/v1/runs/"+runID+"/evidence", nil)
		r.Header.Set("Authorization", "Bearer "+processToken)
		response, err := f.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var record evidence.Record
		err = json.NewDecoder(response.Body).Decode(&record)
		response.Body.Close()
		if response.StatusCode == 200 && err == nil && record.Status == "failed" {
			if record.Bundle != nil || len(record.Manifest) != 0 {
				t.Fatal("failed run has signed success data")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("canceled or timed-out run did not become failed")
}
