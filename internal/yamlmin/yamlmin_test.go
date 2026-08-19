package yamlmin

import (
	"strings"
	"testing"
	"time"
)

type inner struct {
	Enabled bool          `yaml:"enabled"`
	Timeout time.Duration `yaml:"timeout"`
}

type sample struct {
	Name    string            `yaml:"name"`
	Count   int               `yaml:"count"`
	Big     int64             `yaml:"big"`
	Rate    float64           `yaml:"rate"`
	On      bool              `yaml:"on"`
	Maybe   *bool             `yaml:"maybe"`
	Paths   []string          `yaml:"paths"`
	Flow    []string          `yaml:"flow"`
	Headers map[string]string `yaml:"headers"`
	Nested  inner             `yaml:"nested"`
	Skipped string            `yaml:"-"`
}

func mustLoad(t *testing.T, doc string) sample {
	t.Helper()
	var s sample
	if err := Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return s
}

func TestScalarsBindByTargetType(t *testing.T) {
	got := mustLoad(t, `
name: "hello"
count: 42
big: 4194304
rate: 1.5
on: true
nested:
  enabled: false
  timeout: 2s
`)
	if got.Name != "hello" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Count != 42 || got.Big != 4194304 {
		t.Errorf("ints = %d/%d", got.Count, got.Big)
	}
	if got.Rate != 1.5 {
		t.Errorf("rate = %v", got.Rate)
	}
	if !got.On || got.Nested.Enabled {
		t.Errorf("bools = %v/%v", got.On, got.Nested.Enabled)
	}
	if got.Nested.Timeout != 2*time.Second {
		t.Errorf("timeout = %v", got.Nested.Timeout)
	}
}

func TestDurationForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"15s", 15 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"15m", 15 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"0", 0},
		{"1000000000", time.Second}, // bare number is nanoseconds
	} {
		got := mustLoad(t, "nested:\n  timeout: "+tc.in+"\n")
		if got.Nested.Timeout != tc.want {
			t.Errorf("%q -> %v, want %v", tc.in, got.Nested.Timeout, tc.want)
		}
	}
}

func TestSequences(t *testing.T) {
	got := mustLoad(t, `
paths:
  - "/var/log/app/*.log"
  - /plain/path
flow: ["cpu", "memory", plain]
`)
	if len(got.Paths) != 2 || got.Paths[0] != "/var/log/app/*.log" || got.Paths[1] != "/plain/path" {
		t.Errorf("paths = %q", got.Paths)
	}
	if len(got.Flow) != 3 || got.Flow[0] != "cpu" || got.Flow[2] != "plain" {
		t.Errorf("flow = %q", got.Flow)
	}
}

func TestMapAndPointer(t *testing.T) {
	got := mustLoad(t, `
headers:
  x-key: "abc"
  x-other: plain
maybe: false
`)
	if got.Headers["x-key"] != "abc" || got.Headers["x-other"] != "plain" {
		t.Errorf("headers = %v", got.Headers)
	}
	if got.Maybe == nil || *got.Maybe {
		t.Errorf("maybe = %v, want a non-nil false", got.Maybe)
	}
	// Absent pointer stays nil — that distinction is what lets an unset
	// keep_errors default to true rather than to false.
	none := mustLoad(t, "name: x\n")
	if none.Maybe != nil {
		t.Errorf("absent pointer = %v, want nil", none.Maybe)
	}
}

// Comments must not eat content, and a '#' inside quotes is not a comment.
func TestComments(t *testing.T) {
	got := mustLoad(t, `
# leading comment
name: "has # hash inside"   # trailing comment
count: 7 # another
`)
	if got.Name != "has # hash inside" {
		t.Errorf("name = %q — a quoted hash was treated as a comment", got.Name)
	}
	if got.Count != 7 {
		t.Errorf("count = %d", got.Count)
	}
}

func TestQuotingRules(t *testing.T) {
	var s sample
	// Single quotes are literal: backslashes stay. This is exactly how the
	// shipped multiline start_pattern is written.
	if err := Unmarshal([]byte(`name: '^\d{4}-\d{2}-\d{2}'`), &s); err != nil {
		t.Fatalf("single-quoted: %v", err)
	}
	if s.Name != `^\d{4}-\d{2}-\d{2}` {
		t.Errorf("single-quoted = %q", s.Name)
	}

	if err := Unmarshal([]byte(`name: "tab\there"`), &s); err != nil {
		t.Fatalf("double-quoted: %v", err)
	}
	if s.Name != "tab\there" {
		t.Errorf("double-quoted = %q", s.Name)
	}

	if err := Unmarshal([]byte(`name: 'it''s'`), &s); err != nil {
		t.Fatalf("escaped quote: %v", err)
	}
	if s.Name != "it's" {
		t.Errorf("escaped single quote = %q", s.Name)
	}
}

// yes/no/on/off are NOT booleans in YAML 1.2, and the previous library rejected
// them too. Accepting them would resurrect the "Norway problem".
func TestBooleanWordsAreNotBooleans(t *testing.T) {
	for _, word := range []string{"yes", "no", "on", "off", "y", "n"} {
		var s sample
		err := Unmarshal([]byte("on: "+word+"\n"), &s)
		if err == nil {
			t.Errorf("%q was accepted as a boolean; it must not be", word)
		}
	}
	// ...but as a string it is simply itself.
	got := mustLoad(t, "name: no\n")
	if got.Name != "no" {
		t.Errorf("plain 'no' in a string field = %q, want \"no\"", got.Name)
	}
}

func TestUnknownKeysIgnored(t *testing.T) {
	got := mustLoad(t, `
name: keep
unknown_key: 123
nested:
  enabled: true
  future_option: whatever
`)
	if got.Name != "keep" || !got.Nested.Enabled {
		t.Errorf("known keys disturbed by unknown ones: %+v", got)
	}
}

// Constructs outside the supported subset must be refused loudly. Silently
// misreading one of these would change a running agent's configuration.
func TestUnsupportedConstructsRejected(t *testing.T) {
	cases := map[string]string{
		"anchor":       "name: &a x\n",
		"alias":        "name: *a\n",
		"tag":          "name: !!str x\n",
		"block scalar": "name: |\n  text\n",
		"folded":       "name: >\n  text\n",
		"flow mapping": "nested: {enabled: true}\n",
		"multi-doc":    "---\nname: x\n",
		"tab indent":   "nested:\n\tenabled: true\n",
		"unterminated": "name: \"open\n",
	}
	for label, doc := range cases {
		var s sample
		if err := Unmarshal([]byte(doc), &s); err == nil {
			t.Errorf("%s: accepted, want an error", label)
		}
	}
}

func TestErrorsCarryLineNumbers(t *testing.T) {
	var s sample
	err := Unmarshal([]byte("name: ok\ncount: notanumber\n"), &s)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %q does not identify the offending line", err)
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("error %q does not identify the offending key", err)
	}
}

func TestEmptyAndAbsentValues(t *testing.T) {
	got := mustLoad(t, "name:\ncount: 5\n")
	if got.Name != "" || got.Count != 5 {
		t.Errorf("empty value handling: %+v", got)
	}
	// null and ~ are the empty value in YAML's core schema.
	got = mustLoad(t, "name: null\n")
	if got.Name != "" {
		t.Errorf("null -> %q, want empty", got.Name)
	}
}

func TestRejectsNonPointerTarget(t *testing.T) {
	var s sample
	if err := Unmarshal([]byte("name: x"), s); err == nil {
		t.Error("passing a non-pointer was accepted")
	}
}
