package admin

import (
	"context"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

type DoctorReport struct {
	OS             string        `json:"os"`
	Architecture   string        `json:"architecture"`
	GoVersion      string        `json:"go_version"`
	ConfigValid    bool          `json:"config_valid"`
	ActiveModel    string        `json:"active_model,omitempty"`
	AssetIntegrity string        `json:"asset_integrity"`
	Hardware       string        `json:"hardware_acceptance"`
	Note           string        `json:"note"`
	Checks         []DoctorCheck `json:"checks"`
}
type DoctorCheck struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Detected  string `json:"detected,omitempty"`
	Guidance  string `json:"guidance"`
}

func Doctor(configPath string) (DoctorReport, error) {
	return DoctorWith(context.Background(), configPath, OSExecutor{}, func() ([]byte, error) { return os.ReadFile("/etc/os-release") }, runtime.GOOS)
}

// DoctorWith makes environment probes testable without Docker/GPU hardware.
// Successful inventory establishes presence, never compatibility certification.
func DoctorWith(ctx context.Context, configPath string, ex Executor, osRelease func() ([]byte, error), hostOS string) (DoctorReport, error) {
	r := DoctorReport{OS: hostOS, Architecture: runtime.GOARCH, GoVersion: runtime.Version(), Hardware: "unverified", AssetIntegrity: "not_checked", Note: "Read-only metadata; no GPU inference, network isolation or hardware certification was performed."}
	add := func(component, status, detected, guidance string) {
		r.Checks = append(r.Checks, DoctorCheck{component, status, detected, guidance})
	}
	if hostOS != "linux" {
		add("linux_host", "missing", hostOS, "Deploy on Linux; this machine can run development/packaging checks only.")
	} else if b, e := osRelease(); e != nil {
		add("linux_distribution", "missing", "", "Provide /etc/os-release and qualify the OS/kernel against the pinned runtime.")
	} else {
		values := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			kv := strings.SplitN(line, "=", 2)
			if len(kv) == 2 && (kv[0] == "ID" || kv[0] == "VERSION_ID") {
				values[kv[0]] = strings.Trim(kv[1], "\"")
			}
		}
		add("linux_distribution", "unverified", values["ID"]+" "+values["VERSION_ID"], "Compare this exact OS/kernel with the pinned CUDA/ROCm support matrix; Ubuntu branding alone is insufficient.")
	}
	probe := func(args ...string) (string, error) {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		b, e := ex.Run(c, args[0], args[1:]...)
		return strings.TrimSpace(string(b)), e
	}
	if version, e := probe("docker", "version", "--format", "{{.Server.Version}}"); e != nil || version == "" {
		add("docker_daemon", "missing", "", "Install/start a local Docker Engine and grant administrator access; never mount its socket into the gateway.")
	} else {
		add("docker_daemon", "unverified", boundedMetadata(version), "Verify local daemon GPU integration, iptables backend and immutable image availability.")
	}
	if version, e := probe("docker", "compose", "version", "--short"); e != nil {
		add("docker_compose", "missing", "", "Install Docker Compose v2.24+ and validate the selected vendor file.")
	} else {
		parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
		supported := false
		if len(parts) >= 2 {
			major, _ := strconv.Atoi(parts[0])
			minor, _ := strconv.Atoi(parts[1])
			supported = major > 2 || major == 2 && minor >= 24
		}
		status := "unverified"
		if !supported {
			status = "missing"
		}
		add("docker_compose", status, boundedMetadata(version), "Require Compose v2.24+; review compose config and create without starting before applying firewall rules.")
	}
	nvidia, nvErr := probe("nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader,nounits")
	amd, amdErr := probe("rocminfo")
	gfx := regexp.MustCompile(`\bgfx[0-9a-f]+\b`).FindAllString(amd, -1)
	seen := map[string]bool{}
	unique := []string{}
	for _, g := range gfx {
		if !seen[g] {
			seen[g] = true
			unique = append(unique, g)
		}
	}
	if nvErr == nil && nvidia != "" {
		add("nvidia_gpu", "unverified", boundedMetadata(nvidia), "Record GPU/VRAM/driver; qualify the exact CUDA image with real inference and isolation tests.")
	} else {
		status := "missing"
		if amdErr == nil && len(unique) > 0 {
			status = "not_applicable"
		}
		add("nvidia_gpu", status, "", "For NVIDIA, install driver/tools and check nvidia-smi; detection does not establish compatibility.")
	}
	if amdErr == nil && len(unique) > 0 {
		add("amd_gpu", "unverified", strings.Join(unique, ","), "Record GPU/VRAM, kernel, ROCm version and kfd/render permissions; qualify the exact gfx target.")
		if driver, e := probe("rocm-smi", "--showdriverversion", "--json"); e == nil {
			add("amd_driver", "unverified", boundedMetadata(driver), "Check this driver/kernel against the pinned ROCm image; successful inventory is not hardware acceptance.")
		} else {
			add("amd_driver", "missing", "", "Obtain amdgpu driver and ROCm versions from the installed vendor tools before qualification.")
		}
	} else {
		status := "missing"
		if nvErr == nil && nvidia != "" {
			status = "not_applicable"
		}
		add("amd_gpu", status, "", "For AMD, install ROCm inventory tools and confirm rocminfo exposes the selected gfx target.")
	}
	if configPath == "" {
		add("configuration", "missing", "", "Supply --config to check the model catalog and installed asset hashes.")
		return r, nil
	}
	c, e := config.Load(configPath)
	if e != nil {
		add("configuration", "missing", "", "Configuration could not be validated; check schema and readability.")
		return r, nil
	}
	r.ConfigValid = true
	r.ActiveModel = c.ActiveModel
	m, h, e := Verify(c.Active().Path, c.Active().ManifestSHA256)
	if e != nil {
		add("model_assets", "missing", "", "Import a complete bundle with its independently trusted manifest hash.")
		return r, nil
	}
	if MatchModel(c.Active(), m, h) != nil {
		add("model_assets", "missing", "", "Catalog metadata does not match its manifest; reconcile before activation.")
		return r, nil
	}
	r.AssetIntegrity = "verified"
	add("model_assets", "verified", "", "Hashes match; GPU execution and no-egress remain unverified.")
	return r, nil
}
func boundedMetadata(s string) string {
	if len(s) > 1024 {
		s = s[:1024]
	}
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}
