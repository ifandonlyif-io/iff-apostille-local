// apostille-workflow is an offline workflow metadata evidence CLI.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"time"

	workflow "github.com/ifandonlyif-io/iff-apostille-local/internal/workflowevidence"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		if _, err := io.WriteString(stdout, "apostille-workflow: keygen | sign | verify | verify-set\nOffline signed workflow metadata; receiver policy must be independently selected.\n"); err != nil {
			return 1
		}
		return 0
	}
	value, err := execute(args)
	if err != nil {
		// Never echo parser errors, paths, file contents, keys, or raw input.
		_, _ = io.WriteString(stderr, "workflow_operation_failed\n")
		return 1
	}
	if json.NewEncoder(stdout).Encode(value) != nil {
		return 1
	}
	return 0
}

func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}

func parse(f *flag.FlagSet, args []string) error {
	if f.Parse(args) != nil || f.NArg() != 0 {
		return workflow.ErrInvalid
	}
	return nil
}

func policyFile(path string) (workflow.Policy, error) {
	raw, err := workflow.ReadFile(path)
	if err != nil {
		return workflow.Policy{}, err
	}
	return workflow.ParsePolicy(raw)
}

func execute(args []string) (any, error) {
	if len(args) == 0 {
		return nil, workflow.ErrInvalid
	}
	switch args[0] {
	case "keygen":
		f := flags(args[0])
		out := f.String("out-key", "", "exclusive private seed output")
		agent := f.String("agent-id", "", "stable producer UUIDv4")
		if parse(f, args[1:]) != nil || *out == "" || !core.ValidID(*agent) {
			return nil, workflow.ErrInvalid
		}
		pin, err := workflow.GenerateKey(*out)
		if err != nil {
			return nil, err
		}
		return struct {
			AgentID     string `json:"agent_id"`
			ProducerPin string `json:"producer_pin"`
		}{*agent, pin}, nil
	case "sign":
		f := flags(args[0])
		input := f.String("event", "", "event input")
		key := f.String("key-file", "", "private seed input")
		agent := f.String("agent-id", "", "stable producer UUIDv4")
		out := f.String("out", "", "exclusive receipt output")
		artifact := f.String("artifact", "", "explicit optional model artifact")
		if parse(f, args[1:]) != nil || *input == "" || *key == "" || *out == "" || !core.ValidID(*agent) {
			return nil, workflow.ErrInvalid
		}
		raw, err := workflow.ReadFile(*input)
		if err != nil {
			return nil, err
		}
		event, err := workflow.ParseEvent(raw)
		if err != nil || event.AgentID != *agent || event.ArtifactSHA256 != "" || event.ArtifactSize != "" {
			return nil, workflow.ErrInvalid
		}
		if *artifact != "" {
			event, err = workflow.BindArtifact(event, *artifact)
			if err != nil {
				return nil, err
			}
		}
		signer, err := workflow.ReadSigner(*key)
		if err != nil {
			return nil, err
		}
		record, err := workflow.Sign(event, signer, *agent, time.Now())
		if err != nil {
			return nil, err
		}
		raw, err = core.Canonical(record)
		if err != nil || len(raw) > workflow.MaxBytes {
			return nil, workflow.ErrInvalid
		}
		if err = workflow.WriteExclusive(*out, raw); err != nil {
			return nil, err
		}
		return struct {
			Status      string `json:"status"`
			EventID     string `json:"event_id"`
			ProducerPin string `json:"producer_pin"`
		}{"ready", event.EventID, signer.KeyID()}, nil
	case "verify":
		f := flags(args[0])
		receipt := f.String("receipt", "", "receipt input")
		policyPath := f.String("policy", "", "independent receiver policy")
		artifact := f.String("artifact", "", "optional model artifact")
		if parse(f, args[1:]) != nil || *receipt == "" || *policyPath == "" {
			return nil, workflow.ErrInvalid
		}
		policy, err := policyFile(*policyPath)
		if err != nil {
			return nil, err
		}
		raw, err := workflow.ReadFile(*receipt)
		if err != nil {
			return nil, err
		}
		result, err := workflow.Verify(raw, policy)
		if err != nil {
			return nil, err
		}
		if *artifact != "" {
			result, err = workflow.MatchArtifact(result, *artifact)
			if err != nil {
				return nil, err
			}
		}
		return result, nil
	case "verify-set":
		f := flags(args[0])
		directory := f.String("directory", "", "flat receipt archive")
		policyPath := f.String("policy", "", "independent receiver policy")
		if parse(f, args[1:]) != nil || *directory == "" || *policyPath == "" {
			return nil, workflow.ErrInvalid
		}
		policy, err := policyFile(*policyPath)
		if err != nil {
			return nil, err
		}
		return workflow.VerifyDirectory(*directory, policy)
	default:
		return nil, workflow.ErrInvalid
	}
}
