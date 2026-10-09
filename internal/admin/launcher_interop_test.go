package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The deployment launcher is Python, so exercise real Prepare output across the
// language boundary. Python 3 is part of the repository's required test tools;
// absence is a failure rather than silently skipping manifest compatibility.
func TestPreparedManifestLauncherOptionalFieldCompatibility(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for launcher interoperability tests")
	}
	launcher, err := filepath.Abs("../../deploy/runtime_launcher.py")
	if err != nil {
		t.Fatal(err)
	}
	root := tempDirectory(t)
	configPath, c := fixtureConfig(t, root) // Calls actual admin.Prepare.
	m := c.Active()
	manifestRaw, err := os.ReadFile(filepath.Join(m.Path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Model map[string]any `json:"model"`
	}
	if err = json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tool_call_parser", "runtime_profile"} {
		if _, exists := manifest.Model[field]; exists {
			t.Fatalf("fixture did not exercise Go omitempty for %s", field)
		}
	}
	const validate = `import importlib.util,sys
spec = importlib.util.spec_from_file_location("launcher", sys.argv[1])
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)
model = launcher.validate(sys.argv[2], sys.argv[3])
assert "--enable-auto-tool-choice" not in launcher.command(model, sys.argv[3])
`
	for _, fields := range [][]string{nil, {"tool_call_parser"}, {"runtime_profile"}, {"tool_call_parser", "runtime_profile"}} {
		name := "omitted"
		if len(fields) > 0 {
			name = fields[0]
			if len(fields) > 1 {
				name = "both_explicit_empty"
			}
		}
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			var catalog map[string]any
			if err = json.Unmarshal(raw, &catalog); err != nil {
				t.Fatal(err)
			}
			model := catalog["models"].([]any)[0].(map[string]any)
			for _, field := range fields {
				model[field] = ""
			}
			raw, err = json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			put(t, configPath, raw)
			output, err := exec.Command(python, "-c", validate, launcher, configPath, m.Path).CombinedOutput()
			if err != nil {
				t.Fatalf("launcher rejected Go-prepared synthetic bundle: %v\n%s", err, output)
			}
		})
	}
}
