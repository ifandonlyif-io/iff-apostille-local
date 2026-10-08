package workflowevidence

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

type Record struct {
	Event  json.RawMessage `json:"event"`
	Bundle core.Bundle     `json:"bundle"`
}

// Sign records a producer assertion, including explicit failure/cancellation.
// A signed work_failed event never becomes a work_completed event.
func Sign(event Event, signer *core.Signer, agentID string, now time.Time) (Record, error) {
	if event.Validate() != nil || signer == nil || !core.ValidID(agentID) || event.AgentID != agentID || now.IsZero() {
		return Record{}, ErrInvalid
	}
	raw, err := core.Canonical(event)
	if err != nil {
		return Record{}, ErrInvalid
	}
	statement, err := core.CreateStatement(bytes.NewReader(raw), "application/json", signer, nil, agentID, now.UTC())
	if err != nil {
		return Record{}, ErrInvalid
	}
	return Record{Event: raw, Bundle: core.Bundle{Protocol: core.Protocol, Statement: statement}}, nil
}

func ParseRecord(raw []byte) (Record, error) {
	var record Record
	obj, err := exactObject(raw, []string{"event", "bundle"})
	if err != nil {
		return record, err
	}
	event, err := ParseEvent(obj["event"])
	if err != nil {
		return record, err
	}
	canonical, err := core.Canonical(event)
	if err != nil || !bytes.Equal(canonical, obj["event"]) {
		return record, ErrInvalid
	}
	if core.StrictJSON(obj["bundle"], &record.Bundle) != nil {
		return record, ErrInvalid
	}
	// Go's JSON decoder otherwise accepts case aliases and missing fields.
	// Comparing both canonical object forms enforces Core's exact field shape
	// while retaining the three required null producer-only envelope fields.
	var generic any
	if core.StrictJSON(obj["bundle"], &generic) != nil {
		return record, ErrInvalid
	}
	actual, err := core.Canonical(generic)
	if err != nil {
		return record, ErrInvalid
	}
	expected, err := core.Canonical(record.Bundle)
	if err != nil || !bytes.Equal(actual, expected) {
		return record, ErrInvalid
	}
	record.Event = canonical
	return record, nil
}

type Verification struct {
	Valid                bool   `json:"valid"`
	ProducerKeyPolicy    string `json:"producer_key_policy"`
	ProjectID            string `json:"project_id"`
	JobID                string `json:"job_id"`
	EventID              string `json:"event_id"`
	EventType            string `json:"event_type"`
	EvidenceScope        string `json:"evidence_scope"`
	ArtifactBinding      string `json:"artifact_binding"`
	ActualExecution      string `json:"actual_execution"`
	ContentTruth         string `json:"content_truth"`
	CurrentAuthorization string `json:"current_authorization"`
	AgentID              string `json:"-"`
	Event                Event  `json:"-"`
}

func Verify(raw []byte, policy Policy) (Verification, error) {
	var out Verification
	if policy.Validate() != nil {
		return out, ErrInvalid
	}
	record, err := ParseRecord(raw)
	if err != nil {
		return out, err
	}
	event, err := ParseEvent(record.Event)
	if err != nil || event.ProjectID != policy.ProjectID || event.JobID != policy.JobID || record.Bundle.Delegation != nil || record.Bundle.Acceptance != nil || record.Bundle.Certificate != nil {
		return out, ErrInvalid
	}
	bundleRaw, err := core.Canonical(record.Bundle)
	if err != nil {
		return out, ErrInvalid
	}
	verified, err := core.Verify(bundleRaw, core.VerifyOptions{})
	if err != nil || !core.VerifyArtifact(verified, record.Event) || verified.Statement.ArtifactMediaType != "application/json" || verified.Statement.AgentID != event.AgentID {
		return out, ErrInvalid
	}
	allowed := false
	for _, producer := range policy.Producers {
		if producer.AgentID == verified.Statement.AgentID && producer.KeyID == verified.Statement.IssuerKeyID {
			for _, kind := range producer.EventTypes {
				if kind == event.EventType {
					allowed = true
				}
			}
		}
	}
	if !allowed {
		return out, ErrInvalid
	}
	binding := "not_bound"
	if event.ArtifactSHA256 != "" {
		binding = "not_checked"
	}
	return Verification{Valid: true, ProducerKeyPolicy: "matched", ProjectID: event.ProjectID, JobID: event.JobID, EventID: event.EventID, EventType: event.EventType, EvidenceScope: Scope, ArtifactBinding: binding, ActualExecution: "unknown", ContentTruth: "unknown", CurrentAuthorization: "unknown", AgentID: verified.Statement.AgentID, Event: event}, nil
}

type SetVerification struct {
	Valid                bool   `json:"valid"`
	RecordCount          int    `json:"record_count"`
	ProducerKeyPolicy    string `json:"producer_key_policy"`
	ProjectID            string `json:"project_id"`
	JobID                string `json:"job_id"`
	EvidenceScope        string `json:"evidence_scope"`
	ArchiveCompleteness  string `json:"archive_completeness"`
	ActualExecution      string `json:"actual_execution"`
	ContentTruth         string `json:"content_truth"`
	CurrentAuthorization string `json:"current_authorization"`
}

// VerifySet checks the selected archive, not a complete distributed history.
// Removing its last records is undetectable without an independent checkpoint.
func VerifySet(records [][]byte, policy Policy) (SetVerification, error) {
	var out SetVerification
	if len(records) > MaxRecords || policy.Validate() != nil {
		return out, ErrInvalid
	}
	verified := make([]Verification, 0, len(records))
	for _, raw := range records {
		v, err := Verify(raw, policy)
		if err != nil {
			return out, err
		}
		verified = append(verified, v)
	}
	return verifySetValues(verified, policy)
}

func verifySetValues(records []Verification, policy Policy) (SetVerification, error) {
	var out SetVerification
	ids := map[string]bool{}
	agents := map[string][]Verification{}
	for _, v := range records {
		if ids[v.EventID] {
			return out, ErrInvalid
		}
		ids[v.EventID] = true
		agents[v.AgentID] = append(agents[v.AgentID], v)
	}
	for _, events := range agents {
		sort.Slice(events, func(i, j int) bool {
			a, _ := decimal(events[i].Event.Sequence, MaxCounter, false)
			b, _ := decimal(events[j].Event.Sequence, MaxCounter, false)
			return a < b
		})
		rounds := map[string]uint64{}
		for i, v := range events {
			seq, _ := decimal(v.Event.Sequence, MaxCounter, false)
			if seq != uint64(i+1) {
				return out, ErrInvalid
			}
			if workEvent(v.EventType) {
				stream := v.Event.ConfigurationID + ":" + v.Event.ModelID + ":" + v.Event.Framework
				round, _ := decimal(v.Event.Round, MaxCounter, false)
				if round <= rounds[stream] {
					return out, ErrInvalid
				}
				rounds[stream] = round
			}
		}
	}
	return SetVerification{Valid: true, RecordCount: len(records), ProducerKeyPolicy: "matched", ProjectID: policy.ProjectID, JobID: policy.JobID, EvidenceScope: Scope, ArchiveCompleteness: "unknown", ActualExecution: "unknown", ContentTruth: "unknown", CurrentAuthorization: "unknown"}, nil
}
