package collector

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCombinedLine_WithRequestTime(t *testing.T) {
	line := `127.0.0.1 - - [12/Aug/2026:04:05:06 +0000] "GET /api/users HTTP/1.1" 200 1234 "-" "curl/8.0" 0.042`

	event, err := parseCombinedLine(line)
	if err != nil {
		t.Fatalf("parseCombinedLine: %v", err)
	}

	if event.method != "GET" {
		t.Errorf("method = %q, want GET", event.method)
	}
	if event.path != "/api/users" {
		t.Errorf("path = %q, want /api/users", event.path)
	}
	if event.status != 200 {
		t.Errorf("status = %d, want 200", event.status)
	}
	if event.durationMs != 42 {
		t.Errorf("durationMs = %v, want 42 (0.042s -> 42ms)", event.durationMs)
	}
	if event.remoteAddr != "127.0.0.1" {
		t.Errorf("remoteAddr = %q, want 127.0.0.1", event.remoteAddr)
	}

	wantTime := time.Date(2026, time.August, 12, 4, 5, 6, 0, time.UTC)
	if !event.timestamp.Equal(wantTime) {
		t.Errorf("timestamp = %v, want %v", event.timestamp, wantTime)
	}
}

func TestParseCombinedLine_WithoutRequestTime(t *testing.T) {
	// Plain combined format, no trailing request_time field — should still
	// parse everything else, with durationMs left at 0.
	line := `10.0.0.5 - - [12/Aug/2026:04:05:06 +0000] "POST /login HTTP/1.1" 401 89 "https://example.com/" "Mozilla/5.0"`

	event, err := parseCombinedLine(line)
	if err != nil {
		t.Fatalf("parseCombinedLine: %v", err)
	}
	if event.method != "POST" || event.path != "/login" || event.status != 401 {
		t.Errorf("unexpected parse: %+v", event)
	}
	if event.durationMs != 0 {
		t.Errorf("durationMs = %v, want 0 (no request_time field present)", event.durationMs)
	}
}

func TestParseCombinedLine_Malformed(t *testing.T) {
	malformed := []string{
		"",
		"not a log line at all",
		`127.0.0.1 - - [bad-timestamp] "GET / HTTP/1.1" 200 100 "-" "-"`, // bad timestamp still parses (falls back to now) — this one actually should succeed
		`127.0.0.1 - - [12/Aug/2026:04:05:06 +0000] no quotes here 200`,
	}
	for _, line := range malformed {
		_, err := parseCombinedLine(line)
		if line == malformed[2] {
			continue // documented exception above — bad timestamp doesn't fail parsing
		}
		if err == nil {
			t.Errorf("expected parseCombinedLine to reject %q, got no error", line)
		}
	}
}

func TestParseCombinedLine_BadTimestampFallsBackToNow(t *testing.T) {
	line := `127.0.0.1 - - [bad-timestamp] "GET / HTTP/1.1" 200 100 "-" "-"`
	before := time.Now().UTC()
	event, err := parseCombinedLine(line)
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("expected success with fallback timestamp, got error: %v", err)
	}
	if event.timestamp.Before(before) || event.timestamp.After(after) {
		t.Errorf("expected timestamp to fall back to ~now, got %v (window %v..%v)", event.timestamp, before, after)
	}
}

func TestParseJSONLine_DefaultFields(t *testing.T) {
	line := `{"method":"GET","path":"/health","status":200,"duration_ms":3.5,"remote_addr":"10.0.0.1"}`

	event, err := parseJSONLine(line, JSONFieldMap{}.withDefaults())
	if err != nil {
		t.Fatalf("parseJSONLine: %v", err)
	}
	if event.method != "GET" || event.path != "/health" || event.status != 200 {
		t.Errorf("unexpected parse: %+v", event)
	}
	if event.durationMs != 3.5 {
		t.Errorf("durationMs = %v, want 3.5", event.durationMs)
	}
}

func TestParseJSONLine_CustomFieldNames(t *testing.T) {
	line := `{"http_method":"DELETE","route":"/items/42","response_code":204,"latency_ms":12}`

	fields := JSONFieldMap{
		Method:     "http_method",
		Path:       "route",
		Status:     "response_code",
		DurationMs: "latency_ms",
	}.withDefaults()

	event, err := parseJSONLine(line, fields)
	if err != nil {
		t.Fatalf("parseJSONLine: %v", err)
	}
	if event.method != "DELETE" || event.path != "/items/42" || event.status != 204 || event.durationMs != 12 {
		t.Errorf("unexpected parse with custom field names: %+v", event)
	}
}

func TestParseJSONLine_MalformedOrMissingFields(t *testing.T) {
	cases := []string{
		`not json`,
		`{"status":200}`,          // missing method/path
		`{"method":"","path":""}`, // empty required fields
	}
	for _, line := range cases {
		if _, err := parseJSONLine(line, JSONFieldMap{}.withDefaults()); err == nil {
			t.Errorf("expected parseJSONLine to reject %q, got no error", line)
		}
	}
}

// TestAccessLogCollector_TailsAndParsesLiveAppendedLines exercises the
// actual file-tailing + parsing pipeline end to end: writes real combined
// log lines to a file, starts the collector, appends more lines while
// it's running, and confirms the correct Envelopes come out — not just
// that the parser functions work in isolation.
func TestAccessLogCollector_TailsAndParsesLiveAppendedLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")

	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating test log file: %v", err)
	}
	// One line written before the collector starts — should NOT appear
	// (collector starts tailing from EOF, matching the "no historical
	// replay" behavior documented in the code).
	writeLine(t, f, `1.1.1.1 - - [12/Aug/2026:00:00:00 +0000] "GET /before HTTP/1.1" 200 10 "-" "-" 0.001`)
	f.Close()

	reg, err := NewOffsetRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	collector := NewAccessLogCollector("test-agent", []string{logPath}, FormatCombined, JSONFieldMap{}, TailingOptions{
		ScanInterval: time.Second,
		PollInterval: 100 * time.Millisecond,
		MaxLineBytes: 64 * 1024,
		Registry:     reg,
	})
	out := make(chan Envelope, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := collector.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer collector.Stop()

	time.Sleep(600 * time.Millisecond) // let the tailer seek to EOF and start polling

	f, err = os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("reopening log for append: %v", err)
	}
	writeLine(t, f, `2.2.2.2 - - [12/Aug/2026:04:05:06 +0000] "GET /api/orders HTTP/1.1" 200 512 "-" "-" 0.087`)
	writeLine(t, f, `not a valid access log line at all`) // should be silently dropped
	writeLine(t, f, `3.3.3.3 - - [12/Aug/2026:04:05:07 +0000] "POST /api/orders HTTP/1.1" 500 0 "-" "-" 1.204`)
	f.Close()

	var got []Envelope
	timeout := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case env := <-out:
			got = append(got, env)
		case <-timeout:
			t.Fatalf("timed out waiting for envelopes; got %d so far: %+v", len(got), got)
		}
	}

	if len(got) != 2 {
		t.Fatalf("expected exactly 2 valid envelopes (malformed line dropped), got %d", len(got))
	}
	for _, env := range got {
		if env.Kind != KindAPICall {
			t.Errorf("Kind = %q, want api_call", env.Kind)
		}
		if env.Labels["path"] == "/before" {
			t.Error("got the pre-start line — collector should only tail NEW lines from EOF")
		}
	}
	if got[0].Labels["path"] != "/api/orders" || got[0].Value != 87 {
		t.Errorf("first envelope unexpected: labels=%+v value=%v", got[0].Labels, got[0].Value)
	}
	if got[1].Labels["status"] != "500" || got[1].Value != 1204 {
		t.Errorf("second envelope unexpected: labels=%+v value=%v", got[1].Labels, got[1].Value)
	}
}

func writeLine(t *testing.T, f *os.File, line string) {
	t.Helper()
	w := bufio.NewWriter(f)
	if _, err := w.WriteString(line + "\n"); err != nil {
		t.Fatalf("writing test log line: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flushing test log line: %v", err)
	}
}
