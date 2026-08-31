package collector

import (
	"path"
	"strings"
	"testing"
	"time"
)

// newTestLogCollector builds a collector wired to capture instead of send, with
// the tail manager present but never started — handle() is called directly, the
// same way the manager's goroutine would call it.
func newTestLogCollector(t *testing.T, maxLineBytes int) (*DockerLogCollector, *[]Envelope) {
	t.Helper()
	var got []Envelope
	c := NewDockerLogCollector("test-agent", ContainerLogOptions{Root: "/var/lib/docker/containers"},
		TailingOptions{MaxLineBytes: maxLineBytes}, "/nonexistent.sock")
	c.send = func(env Envelope) { got = append(got, env) }
	return c, &got
}

func logLine(id, body string) tailLine {
	return tailLine{
		Path:      path.Join("/var/lib/docker/containers", id, id+"-json.log"),
		Line:      body,
		At:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		FileID:    id,
		EndOffset: int64(len(body)),
	}
}

func TestDockerLogCollector_DecodesJSONFileRecord(t *testing.T) {
	c, got := newTestLogCollector(t, 1024)
	c.meta[testContainerID] = dockerContainer{
		ID: testContainerID, Names: []string{"/api"}, Image: "nginx:1.25",
	}

	c.handle(logLine(testContainerID,
		`{"log":"listening on :8080\n","stream":"stdout","time":"2026-08-27T10:11:12.5Z"}`))

	if len(*got) != 1 {
		t.Fatalf("emitted %d envelopes, want 1", len(*got))
	}
	env := (*got)[0]

	if env.Kind != KindLog {
		t.Errorf("Kind = %v, want %v", env.Kind, KindLog)
	}
	// The trailing newline is the record delimiter, not part of the message.
	if env.Message != "listening on :8080" {
		t.Errorf("Message = %q, want %q", env.Message, "listening on :8080")
	}
	// Source is the container's name, not the unreadable 64-hex path.
	if env.Source != "container/api" {
		t.Errorf("Source = %q, want %q", env.Source, "container/api")
	}
	if env.Labels["stream"] != "stdout" {
		t.Errorf("stream = %q, want stdout", env.Labels["stream"])
	}
	if env.Labels["container.image.name"] != "nginx:1.25" {
		t.Errorf("image = %q, want nginx:1.25", env.Labels["container.image.name"])
	}
	if !strings.HasSuffix(env.Labels["log.file.path"], "-json.log") {
		t.Errorf("log.file.path = %q, want the source file", env.Labels["log.file.path"])
	}

	// Docker's own timestamp, not the moment the agent read the line. After a
	// restart those differ by however long the agent was down, and ordering by
	// read time would interleave the backlog wrongly against everything else.
	want := time.Date(2026, 8, 27, 10, 11, 12, 500_000_000, time.UTC)
	if !env.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v (docker's, not the tailer's)", env.Timestamp, want)
	}

	// Provenance is what lets the exporter commit this file's read offset once
	// the line is delivered. Without it container logs silently lose
	// at-least-once across a crash.
	if env.Labels[LabelTailID] == "" || env.Labels[LabelTailEnd] == "" {
		t.Errorf("tail provenance missing from labels: %v", env.Labels)
	}
}

// Docker splits any line over 16 KiB into several records, marked only by the
// absence of a trailing newline. Without reassembly one long stack trace
// arrives as several unrelated records cut at arbitrary byte boundaries, which
// is worse than truncation because nothing indicates it happened.
func TestDockerLogCollector_ReassemblesChunkedLine(t *testing.T) {
	c, got := newTestLogCollector(t, 1<<20)

	c.handle(logLine(testContainerID, `{"log":"first ","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))
	c.handle(logLine(testContainerID, `{"log":"second ","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))
	if len(*got) != 0 {
		t.Fatalf("emitted %d envelopes mid-line; the chunks should still be buffered", len(*got))
	}

	c.handle(logLine(testContainerID, `{"log":"third\n","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))
	if len(*got) != 1 {
		t.Fatalf("emitted %d envelopes, want 1 reassembled record", len(*got))
	}
	if (*got)[0].Message != "first second third" {
		t.Errorf("Message = %q, want %q", (*got)[0].Message, "first second third")
	}
	// The buffer must be released once the record completes, or a busy
	// container accumulates its whole output in memory.
	if len(c.partial) != 0 {
		t.Errorf("partial buffer retained after the record completed: %v", c.partial)
	}
}

// A stream that never emits a newline must not buffer without bound. The cap is
// the same one the tailer applies to a file with no line breaks.
func TestDockerLogCollector_BoundsUnterminatedLine(t *testing.T) {
	c, got := newTestLogCollector(t, 64)

	for i := 0; i < 20; i++ {
		c.handle(logLine(testContainerID,
			`{"log":"0123456789","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))
	}

	if len(*got) == 0 {
		t.Fatal("nothing emitted; an unterminated stream buffered without limit")
	}
	if len(*got) != 1 {
		t.Fatalf("emitted %d records, want exactly 1 — the remainder of an over-long "+
			"line must be discarded, not emitted as further records", len(*got))
	}
	if !strings.HasSuffix((*got)[0].Message, truncatedSuffix) {
		t.Errorf("truncated record not marked as such: %q", (*got)[0].Message)
	}
	if len((*got)[0].Message) != 64+len(truncatedSuffix) {
		t.Errorf("truncated at %d bytes, want the %d-byte cap plus the marker",
			len((*got)[0].Message)-len(truncatedSuffix), 64)
	}
	if len(c.partial) != 0 {
		t.Errorf("buffer retained after truncation: %v", c.partial)
	}
	// Still discarding: the line has not ended yet.
	if !c.discarding[testContainerID] {
		t.Error("not discarding the remainder of the over-long line")
	}

	// The line finally ends. That chunk is swallowed, and the next one starts a
	// clean record — matching the file tailer's skipToNewline.
	c.handle(logLine(testContainerID, `{"log":"tail end\n","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))
	if len(*got) != 1 {
		t.Errorf("the closing chunk of a discarded line was emitted as a record")
	}
	if c.discarding[testContainerID] {
		t.Error("still discarding after the over-long line ended")
	}

	c.handle(logLine(testContainerID, `{"log":"fresh\n","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))
	if len(*got) != 2 || (*got)[1].Message != "fresh" {
		t.Errorf("the record after a truncated line was not collected cleanly: %+v", *got)
	}
}

// Collecting the agent's own container ships its diagnostics about collecting
// logs back as log data — noise at best, and actively confusing when the thing
// being debugged is log delivery.
func TestDockerLogCollector_SkipsOwnContainer(t *testing.T) {
	c, got := newTestLogCollector(t, 1024)
	c.selfID = testContainerID

	c.handle(logLine(testContainerID, `{"log":"agent noise\n","stream":"stderr","time":"2026-08-27T10:00:00Z"}`))

	if len(*got) != 0 {
		t.Errorf("emitted %d envelopes from the agent's own container", len(*got))
	}
}

// A container the daemon has not been asked about yet — one that started since
// the last metadata refresh — still has to produce logs. Labelling by id until
// the refresh catches up is the degradation; dropping the lines is not.
func TestDockerLogCollector_UnknownContainerFallsBackToID(t *testing.T) {
	c, got := newTestLogCollector(t, 1024)

	c.handle(logLine(testContainerID, `{"log":"early\n","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))

	if len(*got) != 1 {
		t.Fatalf("emitted %d envelopes, want 1", len(*got))
	}
	if (*got)[0].Source != "container/"+testContainerID[:12] {
		t.Errorf("Source = %q, want the short id", (*got)[0].Source)
	}
}

// A file under this path that is not a json-file record is not something the
// agent understands. Forwarding it raw would put unparsed JSON escaping in
// front of whoever reads the logs.
func TestDockerLogCollector_SkipsNonJSONLines(t *testing.T) {
	c, got := newTestLogCollector(t, 1024)

	c.handle(logLine(testContainerID, "this is not json"))
	c.handle(logLine(testContainerID, ""))

	if len(*got) != 0 {
		t.Errorf("emitted %d envelopes for unparseable input", len(*got))
	}
}

func TestDockerLogCollector_IgnoresPathsOutsideAContainerDir(t *testing.T) {
	c, got := newTestLogCollector(t, 1024)

	c.handle(tailLine{
		Path:   "/var/log/syslog",
		Line:   `{"log":"x\n","stream":"stdout","time":"2026-08-27T10:00:00Z"}`,
		FileID: "syslog",
	})

	if len(*got) != 0 {
		t.Errorf("emitted %d envelopes for a path with no container id in it", len(*got))
	}
}

// The buffer is keyed by file, so a host churning containers would otherwise
// accumulate one entry per container that has ever logged a partial line.
func TestDockerLogCollector_ForgetReleasesPartialBuffer(t *testing.T) {
	c, _ := newTestLogCollector(t, 1024)

	c.handle(logLine(testContainerID, `{"log":"unfinished","stream":"stdout","time":"2026-08-27T10:00:00Z"}`))
	if len(c.partial) != 1 {
		t.Fatalf("partial buffer = %v, want one entry", c.partial)
	}

	c.forget(testContainerID)
	if len(c.partial) != 0 {
		t.Errorf("forget() left %v behind", c.partial)
	}
}

// The tail manager re-evaluates this glob on every scan, which is what picks up
// containers created after startup without a discovery loop of its own.
func TestDockerLogCollector_GlobCoversAllContainers(t *testing.T) {
	withHostRoot(t, "/host")
	c := NewDockerLogCollector("test-agent", ContainerLogOptions{}, TailingOptions{}, "")

	globs := c.mgr.opts.globs
	if len(globs) != 1 {
		t.Fatalf("globs = %v, want exactly one", globs)
	}
	if globs[0] != "/host/var/lib/docker/containers/*/*-json.log" {
		t.Errorf("glob = %q, want the host-rooted json-file path", globs[0])
	}
}

// refreshMetadata runs on the tail manager's goroutine, so the time it spends
// waiting on the daemon is time the tailer is not reading files. Against a
// wedged dockerd at the ordinary interval that is a permanent stall of the
// request timeout every refresh period; backing off after a failure is what
// keeps it negligible.
func TestDockerLogCollector_BacksOffAfterAFailedRefresh(t *testing.T) {
	c, _ := newTestLogCollector(t, 1024) // its endpoint does not exist, so every refresh fails

	c.refreshMetadata()
	if !c.metaFailed {
		t.Fatal("a failed refresh was not recorded")
	}
	first := c.lastRefresh

	// Well past the ordinary interval, nowhere near the retry interval.
	c.lastRefresh = time.Now().Add(-(dockerMetaRefresh + time.Second))
	held := c.lastRefresh
	c.refreshMetadata()
	if !c.lastRefresh.Equal(held) {
		t.Error("retried at the ordinary interval after a failure; the backoff is not being applied")
	}

	// Past the retry interval, it tries again.
	c.lastRefresh = time.Now().Add(-(dockerMetaRetry + time.Second))
	c.refreshMetadata()
	if c.lastRefresh.Equal(held) || !c.lastRefresh.After(first) {
		t.Error("did not retry after the backoff elapsed; a daemon that came back would never be noticed")
	}
}

// The ordinary interval must still apply when nothing is wrong, or a healthy
// daemon is polled on every tick — twice a second by default.
func TestDockerLogCollector_RateLimitsHealthyRefresh(t *testing.T) {
	c, _ := newTestLogCollector(t, 1024)
	c.metaFailed = false
	c.lastRefresh = time.Now()
	held := c.lastRefresh

	c.refreshMetadata()
	if !c.lastRefresh.Equal(held) {
		t.Error("refreshed again immediately; the rate limit is not being applied")
	}
}
