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
// It has since been extended once, when the ec2_metadata block was added. That
// regeneration was checked to differ from the frozen baseline in exactly the
// new fields (EC2Metadata, and ResourceAttributes on the exporter) and nothing
// else, so the equivalence the fixture originally proved still holds for every
// field that predates it.
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
