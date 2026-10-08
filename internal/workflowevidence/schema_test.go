package workflowevidence

import (
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestEventSchemaMatchesValidation(t *testing.T) {
	sch, err := jsonschema.NewCompiler().Compile("../../api/workflow-event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	e, _, _ := fixture(t)
	check := func(event Event) {
		t.Helper()
		raw, _ := json.Marshal(event)
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		accepted := sch.Validate(value) == nil
		if accepted != (event.Validate() == nil) {
			t.Fatalf("schema/code disagree: %+v (schema accepted %t)", event, accepted)
		}
	}
	check(e)
	for _, s := range []string{"", "0", "01", "1", "+1", "1.0", "999999999999999", "1000000000000000", "9007199254740990", "9007199254740991", "9007199254740992", "9999999999999999", "1\n"} {
		candidate := e
		candidate.Sequence = s
		check(candidate)
		candidate = e
		candidate.Round = s
		check(candidate)
	}
	for _, s := range []string{"", "0", "00", "01", "1", "1000000000000000000", "9223372036854775806", "9223372036854775807", "9223372036854775808", "18446744073709551615", "1\n"} {
		candidate := e
		candidate.EventType = "model_released"
		candidate.Round = ""
		candidate.ArtifactSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		candidate.ArtifactSize = s
		check(candidate)
	}
	for _, kind := range []string{"configuration_approved", "work_completed", "work_failed", "work_cancelled", "model_released", "deployment_accepted", "other"} {
		candidate := e
		candidate.EventType = kind
		check(candidate)
		candidate.Round = ""
		check(candidate)
	}
	var value map[string]any
	raw, _ := json.Marshal(e)
	_ = json.Unmarshal(raw, &value)
	value["Sequence"] = value["sequence"]
	delete(value, "sequence")
	if sch.Validate(value) == nil {
		t.Fatal("schema accepted case alias")
	}
}

func TestPolicySchema(t *testing.T) {
	sch, err := jsonschema.NewCompiler().Compile("../../api/workflow-policy.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _, p := fixture(t)
	check := func(policy Policy, want bool) {
		t.Helper()
		raw, _ := json.Marshal(policy)
		var value any
		_ = json.Unmarshal(raw, &value)
		if (sch.Validate(value) == nil) != want {
			t.Fatal("policy schema mismatch")
		}
	}
	check(p, true)
	p.Producers[0].EventTypes = append(p.Producers[0].EventTypes, "work_completed")
	check(p, false)
	p.Producers = nil
	check(p, false)
}
