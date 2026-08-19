package config

import (
	"encoding/json"
	"os"
	"testing"
)

// The fixture is the exact JSON serialisation of the Config that gopkg.in/yaml.v3
// produced from the shipped configs/agent.yaml, captured while that library was
// still vendored. Comparing against it is the whole safety argument for
// replacing the parser: the replacement is correct to the extent that it
// reproduces the previous library's output on the real configuration file,
// field for field, rather than merely "parsing without error".
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
