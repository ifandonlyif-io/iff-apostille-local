package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/admin"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
)

func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}
func require(values ...string) error {
	for _, v := range values {
		if v == "" {
			return errors.New("missing_required_flag")
		}
	}
	return nil
}
func emit(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
func abs(p string) string {
	if p == "" {
		return ""
	}
	a, e := filepath.Abs(p)
	if e != nil {
		return p
	}
	return a
}

const help = `Apostille Local administrator CLI (Linux deployment; local filesystem only)

  doctor [--config FILE]                          Read-only metadata and asset check
  key-create --out FILE                           Create owner-only API key; print hash only
  key-hash --file FILE                            Hash existing owner-only API key file
  assets prepare --source DIR --image-archive TAR --out DIR
    --id ID --revision COMMIT --license LICENSE --image REPO@sha256:DIGEST
    [--precision bfloat16] [--max-context 4096] [--max-tokens 512] [--max-concurrent 1]
    [--tool-call-parser hermes]
    [--runtime-profile vllm-chat-v1]
  assets verify --dir DIR --sha256 TRUSTED_HASH
  assets import --source DIR --out DIR --sha256 TRUSTED_HASH [--load-image]
  backup --config FILE --out DIR                  Configuration metadata only; excludes secrets/data
  restore --source DIR --sha256 TRUSTED_HASH --out NEW_CONFIG
  model-activate --config FILE --model ID --compose-file FILE --env-file FILE
    --state-dir DIR [--project-name apostille-local]
    [--ready-url https://localhost:8443/readyz] [--ca-file PEM] [--ready-timeout 10m]

Destinations must not exist. Import requires an independently trusted manifest hash.
The printed key hash goes in projects[].api_key_sha256; deliver the key file privately.
Do not mount the Docker socket or this CLI into the gateway. Hardware is unverified.
`

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(help)
		return nil
	}
	switch args[0] {
	case "doctor":
		f := flags("doctor")
		p := f.String("config", "", "config path")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		r, e := admin.Doctor(*p)
		if e != nil {
			return e
		}
		return emit(r)
	case "key-create", "key-hash":
		f := flags(args[0])
		out := f.String("out", "", "new key file")
		file := f.String("file", "", "existing key file")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		var h string
		var e error
		if args[0] == "key-create" {
			if e = require(*out); e != nil {
				return e
			}
			h, e = admin.KeyCreate(*out)
		} else {
			if e = require(*file); e != nil {
				return e
			}
			h, e = admin.KeyHash(*file)
		}
		if e != nil {
			return e
		}
		return emit(map[string]string{"api_key_sha256": h})
	case "assets":
		if len(args) < 2 {
			return errors.New("assets_subcommand_required")
		}
		f := flags("assets " + args[1])
		source := f.String("source", "", "local source directory")
		out := f.String("out", "", "new destination")
		dir := f.String("dir", "", "bundle directory")
		hash := f.String("sha256", "", "independently trusted manifest hash")
		imageArchive := f.String("image-archive", "", "docker image save archive")
		id := f.String("id", "", "model ID")
		revision := f.String("revision", "", "immutable upstream revision")
		license := f.String("license", "", "model license identifier")
		image := f.String("image", "", "pinned OCI repository digest")
		precision := f.String("precision", "bfloat16", "vLLM dtype")
		maxContext := f.Int("max-context", 4096, "context cap")
		maxTokens := f.Int("max-tokens", 512, "output cap")
		maxConcurrent := f.Int("max-concurrent", 1, "concurrency cap")
		toolParser := f.String("tool-call-parser", "", "explicit tool parser: hermes (default disables tools)")
		runtimeProfile := f.String("runtime-profile", "", "explicit wire adapter: vllm-chat-v1 (default strict)")
		loadImage := f.Bool("load-image", false, "explicitly load local image archive into Docker")
		if e := f.Parse(args[2:]); e != nil {
			return e
		}
		switch args[1] {
		case "prepare":
			if e := require(*source, *out, *imageArchive, *id, *revision, *license, *image); e != nil {
				return e
			}
			m := config.Model{ID: *id, Revision: *revision, License: *license, RuntimeImage: *image, Precision: *precision, MaxContext: *maxContext, MaxTokens: *maxTokens, MaxConcurrent: *maxConcurrent, ToolCallParser: *toolParser, RuntimeProfile: *runtimeProfile}
			imageID, e := admin.ResolveImageID(ctx, admin.OSExecutor{}, *image)
			if e != nil {
				return e
			}
			h, e := admin.Prepare(*source, *imageArchive, *out, m, imageID)
			if e != nil {
				return e
			}
			return emit(map[string]string{"manifest_sha256": h, "hardware_acceptance": "unverified"})
		case "verify":
			if e := require(*dir, *hash); e != nil {
				return e
			}
			m, h, e := admin.Verify(*dir, *hash)
			if e != nil {
				return e
			}
			return emit(map[string]string{"model_id": m.Model.ID, "manifest_sha256": h, "asset_integrity": "verified", "hardware_acceptance": "unverified"})
		case "import":
			if e := require(*source, *out, *hash); e != nil {
				return e
			}
			m, e := admin.Import(*source, *out, *hash)
			if e != nil {
				return e
			}
			if *loadImage {
				if e = admin.LoadImage(ctx, admin.OSExecutor{}, *out, m); e != nil {
					return e
				}
			}
			return emit(map[string]string{"model_id": m.Model.ID, "manifest_sha256": *hash, "image_load": "optional; runtime image availability is checked during activation"})
		default:
			return errors.New("unknown_assets_subcommand")
		}
	case "backup":
		f := flags("backup")
		p := f.String("config", "", "config path")
		out := f.String("out", "", "new backup directory")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if e := require(*p, *out); e != nil {
			return e
		}
		h, e := admin.Backup(*p, *out)
		if e != nil {
			return e
		}
		return emit(map[string]string{"backup_sha256": h, "kind": "configuration-metadata-only"})
	case "restore":
		f := flags("restore")
		source := f.String("source", "", "backup directory")
		out := f.String("out", "", "new config path")
		hash := f.String("sha256", "", "trusted backup metadata hash")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if e := require(*source, *out, *hash); e != nil {
			return e
		}
		if e := admin.Restore(*source, *hash, *out); e != nil {
			return e
		}
		return emit(map[string]string{"configuration": "restored", "secrets": "provision independently before starting services"})
	case "model-activate":
		f := flags("model-activate")
		p := f.String("config", "", "config path")
		model := f.String("model", "", "model ID")
		compose := f.String("compose-file", "", "single vendor compose file")
		env := f.String("env-file", "", "deployment env file")
		state := f.String("state-dir", "", "administrator-only state directory")
		project := f.String("project-name", "apostille-local", "compose project")
		readyURL := f.String("ready-url", "https://localhost:8443/readyz", "loopback TLS readiness URL")
		ca := f.String("ca-file", "", "trust root for local certificate")
		timeout := f.Duration("ready-timeout", 10*time.Minute, "bounded runtime warmup and gateway readiness wait")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if e := require(*p, *model, *compose, *env, *state); e != nil {
			return e
		}
		ready, e := admin.HTTPSReady(*readyURL, *ca)
		if e != nil {
			return e
		}
		o := admin.ActivateOptions{ConfigPath: abs(*p), ModelID: *model, ComposeFile: abs(*compose), EnvironmentFile: abs(*env), StateDirectory: abs(*state), ProjectName: *project, ReadyURL: *readyURL, CAFile: *ca, ReadyTimeout: *timeout}
		if e = admin.Activate(ctx, o, admin.OSExecutor{}, ready); e != nil {
			return e
		}
		return emit(map[string]string{"active_model": *model, "readiness": "passed", "hardware_acceptance": "unverified"})
	default:
		return errors.New("unknown_command; run apostille-local-admin help")
	}
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}
