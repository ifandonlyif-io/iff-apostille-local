// Package workflowevidence signs bounded workflow metadata using Apostille Core
// 0.1. It does not execute training or establish the truth of a producer claim.
package workflowevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"

	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

const (
	EventSchema  = "urn:apostille:workflow-event:0.1"
	PolicySchema = "urn:apostille:workflow-policy:0.1"
	Scope        = "workflow_metadata_only"
	MaxBytes     = 64 << 10
	MaxRecords   = 10000
	MaxCounter   = uint64(1<<53 - 1)
)

var (
	ErrInvalid       = errors.New("invalid_workflow_evidence")
	ErrIO            = errors.New("workflow_io_error")
	frameworkPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	versionPattern   = regexp.MustCompile(`^[0-9][A-Za-z0-9.+_-]{0,31}$`)
	hexPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	pinPattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

var eventFields = []string{"schema", "evidence_scope", "project_id", "job_id", "agent_id", "event_id", "configuration_id", "model_id", "sequence", "round", "event_type", "framework", "framework_version", "artifact_sha256", "artifact_size"}

type Event struct {
	Schema           string `json:"schema"`
	EvidenceScope    string `json:"evidence_scope"`
	ProjectID        string `json:"project_id"`
	JobID            string `json:"job_id"`
	AgentID          string `json:"agent_id"`
	EventID          string `json:"event_id"`
	ConfigurationID  string `json:"configuration_id"`
	ModelID          string `json:"model_id"`
	Sequence         string `json:"sequence"`
	Round            string `json:"round"`
	EventType        string `json:"event_type"`
	Framework        string `json:"framework"`
	FrameworkVersion string `json:"framework_version"`
	ArtifactSHA256   string `json:"artifact_sha256"`
	ArtifactSize     string `json:"artifact_size"`
}

func EventTypeValid(kind string) bool {
	switch kind {
	case "configuration_approved", "work_completed", "work_failed", "work_cancelled", "model_released", "deployment_accepted":
		return true
	}
	return false
}

func workEvent(kind string) bool {
	return kind == "work_completed" || kind == "work_failed" || kind == "work_cancelled"
}

func decimal(s string, maximum uint64, zero bool) (uint64, bool) {
	if s == "" || len(s) > 20 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil && n <= maximum && (zero || n > 0)
}

func (e Event) Validate() error {
	if e.Schema != EventSchema || e.EvidenceScope != Scope || !core.ValidID(e.ProjectID) || !core.ValidID(e.JobID) || !core.ValidID(e.AgentID) || !core.ValidID(e.EventID) || !core.ValidID(e.ConfigurationID) || !core.ValidID(e.ModelID) || !EventTypeValid(e.EventType) || !frameworkPattern.MatchString(e.Framework) || !versionPattern.MatchString(e.FrameworkVersion) {
		return ErrInvalid
	}
	if _, ok := decimal(e.Sequence, MaxCounter, false); !ok {
		return ErrInvalid
	}
	if workEvent(e.EventType) {
		if _, ok := decimal(e.Round, MaxCounter, false); !ok {
			return ErrInvalid
		}
	} else if e.Round != "" {
		return ErrInvalid
	}
	if e.ArtifactSHA256 == "" && e.ArtifactSize == "" {
		return nil
	}
	if (e.EventType != "model_released" && e.EventType != "deployment_accepted") || !hexPattern.MatchString(e.ArtifactSHA256) {
		return ErrInvalid
	}
	if _, ok := decimal(e.ArtifactSize, 1<<63-1, true); !ok {
		return ErrInvalid
	}
	return nil
}

func exactObject(raw []byte, fields []string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if len(raw) > MaxBytes || core.StrictJSON(raw, &obj) != nil || len(obj) != len(fields) {
		return nil, ErrInvalid
	}
	for _, key := range fields {
		if _, ok := obj[key]; !ok {
			return nil, ErrInvalid
		}
	}
	return obj, nil
}

func ParseEvent(raw []byte) (Event, error) {
	var e Event
	obj, err := exactObject(raw, eventFields)
	if err != nil {
		return e, err
	}
	for _, value := range obj {
		var s string
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &s) != nil {
			return e, ErrInvalid
		}
	}
	if core.StrictJSON(raw, &e) != nil || e.Validate() != nil {
		return Event{}, ErrInvalid
	}
	return e, nil
}

type Producer struct {
	AgentID    string   `json:"agent_id"`
	KeyID      string   `json:"key_id"`
	EventTypes []string `json:"event_types"`
}

type Policy struct {
	Schema    string     `json:"schema"`
	ProjectID string     `json:"project_id"`
	JobID     string     `json:"job_id"`
	Producers []Producer `json:"producers"`
}

func (p Policy) Validate() error {
	if p.Schema != PolicySchema || !core.ValidID(p.ProjectID) || !core.ValidID(p.JobID) || len(p.Producers) == 0 || len(p.Producers) > 128 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, producer := range p.Producers {
		key := producer.AgentID + ":" + producer.KeyID
		if !core.ValidID(producer.AgentID) || !pinPattern.MatchString(producer.KeyID) || seen[key] || len(producer.EventTypes) == 0 || len(producer.EventTypes) > 6 {
			return ErrInvalid
		}
		seen[key] = true
		permissions := map[string]bool{}
		for _, kind := range producer.EventTypes {
			if !EventTypeValid(kind) || permissions[kind] {
				return ErrInvalid
			}
			permissions[kind] = true
		}
	}
	return nil
}

func ParsePolicy(raw []byte) (Policy, error) {
	var p Policy
	obj, err := exactObject(raw, []string{"schema", "project_id", "job_id", "producers"})
	if err != nil {
		return p, err
	}
	var producers []json.RawMessage
	if json.Unmarshal(obj["producers"], &producers) != nil {
		return p, ErrInvalid
	}
	for _, rawProducer := range producers {
		if _, err = exactObject(rawProducer, []string{"agent_id", "key_id", "event_types"}); err != nil {
			return p, err
		}
	}
	if core.StrictJSON(raw, &p) != nil || p.Validate() != nil {
		return Policy{}, ErrInvalid
	}
	return p, nil
}
