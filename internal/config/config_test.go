package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func validConfig() Config {
	m := Model{ID: "synthetic", Revision: strings.Repeat("a", 40), ManifestSHA256: strings.Repeat("b", 64), License: "Apache-2.0", RuntimeImage: "example/runtime@sha256:" + strings.Repeat("c", 64), Precision: "float16", MaxContext: 4096, MaxTokens: 128, MaxConcurrent: 2, Path: "/synthetic/model"}
	return Config{Version: 1, Listen: "127.0.0.1:8443", RuntimeURL: "http://runtime:8000", ActiveModel: m.ID, Models: []Model{m}, Projects: []Project{{ID: "project-a", APIKeySHA256: strings.Repeat("d", 64), Models: []string{m.ID}, MaxConcurrent: 1}}, TimeoutSeconds: 60}
}

func TestRuntimeImageContract(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("c", 64)
	for _, image := range []string{"example/runtime" + digest, "registry.example:5000/team/runtime:v1" + digest, strings.Repeat("a", 191) + digest} {
		c := validConfig()
		c.Models[0].RuntimeImage = image
		if !ValidRuntimeImage(image) || c.Validate() != nil {
			t.Errorf("valid digest-pinned image rejected: %q", image)
		}
	}
	for _, image := range []string{"", "runtime:latest", digest, "https://example/runtime" + digest, "example/runtime?token=value" + digest, "example/runtime with space" + digest, "user@registry/runtime" + digest, strings.Repeat("a", 192) + digest, "example/runtime@sha256:" + strings.Repeat("A", 64), "example/runtime@sha256:" + strings.Repeat("c", 63)} {
		c := validConfig()
		c.Models[0].RuntimeImage = image
		if ValidRuntimeImage(image) || c.Validate() == nil {
			t.Errorf("invalid image accepted: %q", image)
		}
	}
}

func TestRuntimeURLIsLocalAndHasNoRoutingInputs(t *testing.T) {
	for _, url := range []string{"http://runtime:8000", "http://runtime", "http://localhost:8000/", "http://127.0.0.1:8000", "http://[::1]:8000", "http://10.0.0.2:8000", "http://[fd00::2]:8000"} {
		c := validConfig()
		c.RuntimeURL = url
		if err := c.Validate(); err != nil {
			t.Errorf("valid local runtime rejected: %q: %v", url, err)
		}
	}
	for _, url := range []string{"https://runtime:8000", "http://example.com", "http://8.8.8.8", "http://169.254.169.254", "http://0.0.0.0:8000", "http://user:pass@runtime:8000", "http://runtime:8000/v1", "http://runtime:8000/%2f", "http://runtime:8000?token=value", "http://runtime:8000?", "http://runtime:8000#fragment", "http://runtime:8000#", "http://runtime:0", "http://runtime:65536", "http://runtime:", "http://runtime:bad", "http:runtime"} {
		c := validConfig()
		c.RuntimeURL = url
		if c.Validate() == nil {
			t.Errorf("invalid runtime accepted: %q", url)
		}
	}
}

func TestModelProjectAndCredentialIsolation(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"duplicate model": func(c *Config) { c.Models = append(c.Models, c.Models[0]) },
		"duplicate project": func(c *Config) {
			p := c.Projects[0]
			p.APIKeySHA256 = strings.Repeat("e", 64)
			c.Projects = append(c.Projects, p)
		},
		"duplicate credential": func(c *Config) {
			p := c.Projects[0]
			p.ID = "project-b"
			c.Projects = append(c.Projects, p)
		},
		"unknown active model":        func(c *Config) { c.ActiveModel = "unknown" },
		"unknown project model":       func(c *Config) { c.Projects[0].Models = []string{"unknown"} },
		"empty project models":        func(c *Config) { c.Projects[0].Models = nil },
		"invalid project key":         func(c *Config) { c.Projects[0].APIKeySHA256 = "synthetic-token" },
		"invalid project ID":          func(c *Config) { c.Projects[0].ID = "../project-a" },
		"unknown tool parser":         func(c *Config) { c.Models[0].ToolCallParser = "custom_plugin.py" },
		"tool parser without adapter": func(c *Config) { c.Models[0].ToolCallParser = "hermes" },
		"unknown runtime profile":     func(c *Config) { c.Models[0].RuntimeProfile = "arbitrary-backend" },
	} {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	c := validConfig()
	p := c.Projects[0]
	p.ID, p.APIKeySHA256 = "project-b", strings.Repeat("e", 64)
	c.Projects = append(c.Projects, p)
	if err := c.Validate(); err != nil {
		t.Fatal("independent project rejected", err)
	}
}

func TestToolParserIsExplicitAndBounded(t *testing.T) {
	for _, pair := range [][2]string{{"", ""}, {"", "vllm-chat-v1"}, {"hermes", "vllm-chat-v1"}} {
		c := validConfig()
		c.Models[0].ToolCallParser = pair[0]
		c.Models[0].RuntimeProfile = pair[1]
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeProfilesAreExplicit(t *testing.T) {
	for _, profile := range []string{"", "vllm-chat-v1"} {
		c := validConfig()
		c.Models[0].RuntimeProfile = profile
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvidenceRequiresCoreAgentIDOnlyWhenEnabled(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatal("disabled evidence rejected", err)
	}
	c.Evidence = Evidence{Directory: "/synthetic/evidence", KeyFile: "/synthetic/seed", AgentID: "11111111-1111-4111-8111-111111111111"}
	if err := c.Validate(); err != nil {
		t.Fatal("valid agent rejected", err)
	}
	for _, agent := range []string{"", "agent-name", "11111111-1111-1111-8111-111111111111", "11111111-1111-4111-1111-111111111111"} {
		c.Evidence.AgentID = agent
		if c.Validate() == nil {
			t.Errorf("invalid Core agent UUID accepted: %q", agent)
		}
	}
	c.Evidence.AgentID = "11111111-1111-4111-8111-111111111111"
	c.Evidence.KeyFile = ""
	if c.Validate() == nil {
		t.Fatal("enabled evidence without key file accepted")
	}
}

func TestEvidenceRequirePostQuantumIsParsed(t *testing.T) {
	var c Evidence
	raw := `{"directory":"/d","key_file":"/k","agent_id":"11111111-1111-4111-8111-111111111111","require_post_quantum":true}`
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil || !c.RequirePostQuantum {
		t.Fatal(c, err)
	}
}
