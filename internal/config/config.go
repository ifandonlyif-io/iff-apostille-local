package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$`)
var hex64 = regexp.MustCompile(`^[a-f0-9]{64}$`)
var revision = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
var runtimeImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,190}@sha256:[a-f0-9]{64}$`)

type Model struct {
	ID             string `json:"id"`
	Revision       string `json:"revision"`
	ManifestSHA256 string `json:"manifest_sha256"`
	License        string `json:"license"`
	RuntimeImage   string `json:"runtime_image"`
	Precision      string `json:"precision"`
	MaxContext     int    `json:"max_context"`
	MaxTokens      int    `json:"max_tokens"`
	MaxConcurrent  int    `json:"max_concurrent"`
	Path           string `json:"path"`
	// ToolCallParser is part of the pinned asset manifest. Empty disables tools;
	// hermes requires vllm-chat-v1 to normalize vLLM's named-call finish reason.
	ToolCallParser string `json:"tool_call_parser,omitempty"`
	// RuntimeProfile selects an explicit wire adapter, pinned with the image
	// and model metadata. Empty keeps the original strict response contract.
	RuntimeProfile string `json:"runtime_profile,omitempty"`
}
type Project struct {
	ID            string   `json:"id"`
	APIKeySHA256  string   `json:"api_key_sha256"`
	Models        []string `json:"models"`
	MaxConcurrent int      `json:"max_concurrent"`
}
type Evidence struct {
	Directory string `json:"directory"`
	KeyFile   string `json:"key_file"`
	AgentID   string `json:"agent_id"`
}
type Config struct {
	Version        int       `json:"version"`
	Listen         string    `json:"listen"`
	TLSCertFile    string    `json:"tls_cert_file"`
	TLSKeyFile     string    `json:"tls_key_file"`
	RuntimeURL     string    `json:"runtime_url"`
	ActiveModel    string    `json:"active_model"`
	Models         []Model   `json:"models"`
	Projects       []Project `json:"projects"`
	Evidence       Evidence  `json:"evidence"`
	TimeoutSeconds int       `json:"timeout_seconds"`
}

func ValidID(s string) bool { return identifier.MatchString(s) }

// ValidRuntimeImage is the shared bounded metadata contract for configuration
// and signed run manifests. References must be digest-pinned and contain no URL
// credentials or query. Image availability is checked separately by deployment.
func ValidRuntimeImage(s string) bool {
	return runtimeImage.MatchString(s) && !strings.Contains(s, "://")
}

func Load(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, errors.New("config_unreadable")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1048577))
	if e != nil || len(b) > 1048576 {
		return c, errors.New("config_too_large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return c, errors.New("invalid_config")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("invalid_config")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	fail := func() error { return errors.New("invalid_config") }
	if c.Version != 1 || len(c.Projects) == 0 || len(c.Models) == 0 || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 600 {
		return fail()
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fail()
	}
	u, e := url.Parse(c.RuntimeURL)
	if e != nil || u.Scheme != "http" || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(c.RuntimeURL, "#") || (u.Path != "" && u.Path != "/") {
		return fail()
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fail()
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return fail()
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "runtime" && host != "localhost" && (ip == nil || (!ip.IsLoopback() && !ip.IsPrivate())) {
		return errors.New("runtime_must_be_local")
	}
	models := map[string]bool{}
	for _, m := range c.Models {
		if m.RuntimeProfile != "" && m.RuntimeProfile != "vllm-chat-v1" {
			return fail()
		}
		if m.ToolCallParser != "" && m.ToolCallParser != "hermes" {
			return fail()
		}
		if m.ToolCallParser == "hermes" && m.RuntimeProfile != "vllm-chat-v1" {
			return fail()
		}
		if !ValidID(m.ID) || models[m.ID] || !revision.MatchString(m.Revision) || !hex64.MatchString(m.ManifestSHA256) || !ValidRuntimeImage(m.RuntimeImage) || m.License == "" || m.Path == "" || m.MaxContext < 128 || m.MaxContext > 131072 || m.MaxTokens < 1 || m.MaxTokens >= m.MaxContext || m.MaxConcurrent < 1 || m.MaxConcurrent > 32 || (m.Precision != "bfloat16" && m.Precision != "float16") {
			return fail()
		}
		models[m.ID] = true
	}
	if !models[c.ActiveModel] {
		return fail()
	}
	projects := map[string]bool{}
	keys := map[string]bool{}
	for _, p := range c.Projects {
		if !ValidID(p.ID) || projects[p.ID] || keys[p.APIKeySHA256] || !hex64.MatchString(p.APIKeySHA256) || len(p.Models) == 0 || p.MaxConcurrent < 1 || p.MaxConcurrent > 32 {
			return fail()
		}
		projects[p.ID] = true
		keys[p.APIKeySHA256] = true
		for _, id := range p.Models {
			if !models[id] {
				return fail()
			}
		}
	}
	if c.Evidence.Directory != "" && (c.Evidence.KeyFile == "" || !core.ValidID(c.Evidence.AgentID)) {
		return fail()
	}
	return nil
}
func (c Config) Active() Model {
	for _, m := range c.Models {
		if m.ID == c.ActiveModel {
			return m
		}
	}
	return Model{}
}
