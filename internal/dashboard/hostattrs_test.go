package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The dashboard has to answer "which machine am I looking at", not just "what
// did someone name this agent". Without these the local view shows only the
// configured agent_id, which is the same on every host cloned from one image.
func TestSnapshot_CarriesHostAttributes(t *testing.T) {
	s := NewStore("ec2-prod-2", "v1", time.Minute, 100)
	s.SetHostAttributes(map[string]string{
		"cloud.provider":          "aws",
		"cloud.platform":          "aws_ec2",
		"cloud.account.id":        "123456789012",
		"cloud.region":            "us-east-1",
		"cloud.availability_zone": "us-east-1a",
		"host.id":                 "i-0123456789abcdef0",
		"host.type":               "t3.medium",
	})

	snap := s.Snapshot()
	if snap.Host["host.id"] != "i-0123456789abcdef0" {
		t.Errorf("host.id = %q", snap.Host["host.id"])
	}
	if snap.Host["host.type"] != "t3.medium" {
		t.Errorf("host.type = %q", snap.Host["host.type"])
	}
	if snap.Host["cloud.region"] != "us-east-1" {
		t.Errorf("cloud.region = %q", snap.Host["cloud.region"])
	}
	// agent_id keeps its own meaning: the name an operator chose.
	if snap.AgentID != "ec2-prod-2" {
		t.Errorf("agent_id = %q, want the configured name to survive", snap.AgentID)
	}
}

// Off a cloud host nothing is discovered, and the field must vanish from the
// payload rather than appear as an empty object the UI has to special-case.
func TestSnapshot_HostOmittedWhenNothingDiscovered(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 100)

	snap := s.Snapshot()
	if snap.Host != nil {
		t.Errorf("Host = %v, want nil", snap.Host)
	}

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"host"`) {
		t.Errorf("payload carries a host key with nothing to report: %s", b)
	}
}

// Setting an empty map must not create an empty object either.
func TestSetHostAttributes_EmptyIsANoOp(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 100)
	s.SetHostAttributes(map[string]string{})
	s.SetHostAttributes(nil)

	if snap := s.Snapshot(); snap.Host != nil {
		t.Errorf("Host = %v, want nil", snap.Host)
	}
}

// A snapshot is handed to an HTTP handler; mutating it must not reach back into
// the store and change what the next request sees.
func TestSnapshot_HostAttributesAreCopied(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 100)
	original := map[string]string{"host.id": "i-abc"}
	s.SetHostAttributes(original)

	// Mutating the caller's map must not affect the store.
	original["host.id"] = "i-mutated"
	if got := s.Snapshot().Host["host.id"]; got != "i-abc" {
		t.Errorf("store followed the caller's map: host.id = %q", got)
	}

	// Mutating a returned snapshot must not affect the store either.
	snap := s.Snapshot()
	snap.Host["host.id"] = "i-clobbered"
	if got := s.Snapshot().Host["host.id"]; got != "i-abc" {
		t.Errorf("snapshot aliased store state: host.id = %q", got)
	}
}

// The field is additive, so the payload a current UI already reads must be
// unchanged in every other respect.
func TestSnapshot_HostDoesNotDisturbTheRestOfThePayload(t *testing.T) {
	s := NewStore("ec2-prod-2", "v9", time.Minute, 100)
	s.SetHostAttributes(map[string]string{"host.id": "i-abc"})

	snap := s.Snapshot()
	if snap.AdapterContract != AdapterContract {
		t.Errorf("adapter contract changed: %q", snap.AdapterContract)
	}
	if snap.Version != "v9" {
		t.Errorf("version = %q", snap.Version)
	}
	if snap.Series == nil || snap.Logs == nil || snap.Spans == nil {
		t.Error("series/logs/spans must stay non-nil so they encode as arrays")
	}
}
