package config

import (
	"encoding/json"
	"os"
	"testing"
)

// The fixture began as the exact JSON serialisation of the Config that
// gopkg.in/yaml.v3 produced from the shipped configs/agent.yaml, captured while
// that library was still vendored. That is what made replacing the parser safe:
// the replacement had to reproduce the previous library's output on the real
// configuration file field for field, not merely parse without error.
//
// It has since been regenerated four times. First when the ec2_metadata block was
// added, differing in exactly the new fields (EC2Metadata, and
// ResourceAttributes on the exporter). Then when agent_id stopped being a
// required field with a hardcoded value in the shipped config, differing in
// exactly AgentID, which went from "host-001" to "". Third when the OTLP
// receiver learned to serve logs and metrics, differing in exactly the two new
// Traces fields, AcceptLogs and AcceptMetrics. Fourth when journald collection
// was added, differing in exactly the new Journald block. Each regeneration was
// diffed against the previous fixture and confirmed to change nothing else, so
// the equivalence the fixture originally proved still holds for every field
// that predates it.
//
// Going forward it serves as a regression test: any unintended change in how
// the real configuration file parses shows up here as a diff.
func TestLoad_MatchesPreviousParserOutput(t *testing.T) {
	want, err := os.ReadFile("testdata/golden_config.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	cfg, err := Load("../../configs/agent.yaml")
	if err != nil {
		t.Fatalf("loading agent.yaml: %v", err)
	}
	got, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("parsed config differs from the previous parser's output.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}
