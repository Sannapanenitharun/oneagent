package collector

import (
	"strings"
	"testing"
	"time"
)

// mlHarness drives an assembler with a controllable clock and records what it
// emits.
type mlHarness struct {
	a   *multilineAssembler
	out []tailLine
	now time.Time
}

func newMLHarness(t *testing.T, opts MultilineOptions) *mlHarness {
	t.Helper()
	h := &mlHarness{now: time.Unix(1700000000, 0).UTC()}
	a, err := newMultilineAssembler(opts, func(ln tailLine) { h.out = append(h.out, ln) })
	if err != nil {
		t.Fatalf("newMultilineAssembler: %v", err)
	}
	a.now = func() time.Time { return h.now }
	h.a = a
	return h
}

func (h *mlHarness) line(fileID, text string, end int64) {
	h.a.handle(tailLine{
		Path:      "/var/log/app.log",
		Line:      text,
		At:        h.now,
		FileID:    fileID,
		EndOffset: end,
	})
}

func (h *mlHarness) messages() []string {
	var out []string
	for _, l := range h.out {
		out = append(out, l.Line)
	}
	return out
}

const tsPattern = `^\d{4}-\d{2}-\d{2}`

func TestMultiline_JoinsAStackTraceIntoOneRecord(t *testing.T) {
	h := newMLHarness(t, MultilineOptions{StartPattern: tsPattern})

	h.line("f1", "2026-08-17 ERROR boom", 10)
	h.line("f1", "  at com.example.Thing.run(Thing.java:42)", 20)
	h.line("f1", "  at com.example.Other.go(Other.java:7)", 30)
	h.line("f1", "2026-08-17 INFO recovered", 40)

	got := h.messages()
	if len(got) != 1 {
		t.Fatalf("emitted %d records (%q), want 1 — the second timestamp should have closed the first", len(got), got)
	}
	want := "2026-08-17 ERROR boom\n  at com.example.Thing.run(Thing.java:42)\n  at com.example.Other.go(Other.java:7)"
	if got[0] != want {
		t.Errorf("record = %q\nwant %q", got[0], want)
	}
	// The record's position must cover every byte it absorbed, or the frames
	// would be read again after a restart.
	if h.out[0].EndOffset != 30 {
		t.Errorf("EndOffset = %d, want 30 (the last line it absorbed)", h.out[0].EndOffset)
	}
}

func TestMultiline_RecordTakesTheTimestampOfItsFirstLine(t *testing.T) {
	h := newMLHarness(t, MultilineOptions{StartPattern: tsPattern})

	start := h.now
	h.line("f1", "2026-08-17 ERROR boom", 10)
	h.now = h.now.Add(3 * time.Second)
	h.line("f1", "  at Thing.run()", 20)
	h.now = h.now.Add(3 * time.Second)
	h.line("f1", "2026-08-17 INFO next", 30)

	if len(h.out) != 1 {
		t.Fatalf("want 1 record, got %d", len(h.out))
	}
	if !h.out[0].At.Equal(start) {
		t.Errorf("record timestamp = %v, want %v — an event happened when it began, not when its last frame was written",
			h.out[0].At, start)
	}
}

func TestMultiline_IdleTimeoutReleasesTheLastRecord(t *testing.T) {
	h := newMLHarness(t, MultilineOptions{StartPattern: tsPattern, Timeout: 2 * time.Second})

	h.line("f1", "2026-08-17 ERROR boom", 10)
	h.line("f1", "  at Thing.run()", 20)

	h.a.flushIdle()
	if len(h.out) != 0 {
		t.Fatalf("record released early: %q", h.messages())
	}

	// Without this the final stack trace in a quiet file is held forever,
	// waiting for a successor that never arrives.
	h.now = h.now.Add(3 * time.Second)
	h.a.flushIdle()

	got := h.messages()
	if len(got) != 1 {
		t.Fatalf("after the timeout, emitted %d records, want 1", len(got))
	}
	if !strings.Contains(got[0], "at Thing.run()") {
		t.Errorf("released record lost its continuation line: %q", got[0])
	}
}

func TestMultiline_FilesDoNotBleedIntoEachOther(t *testing.T) {
	h := newMLHarness(t, MultilineOptions{StartPattern: tsPattern})

	h.line("f1", "2026-08-17 A start", 10)
	h.line("f2", "2026-08-17 B start", 10)
	h.line("f1", "  a-continued", 20)
	h.line("f2", "  b-continued", 20)
	h.a.flushAll()

	got := h.messages()
	if len(got) != 2 {
		t.Fatalf("emitted %d records, want 2", len(got))
	}
	for _, rec := range got {
		if strings.Contains(rec, "a-continued") && strings.Contains(rec, "b-continued") {
			t.Fatalf("two files' lines ended up in one record: %q", rec)
		}
		if strings.Contains(rec, "A start") && strings.Contains(rec, "b-continued") {
			t.Fatalf("file B's line was appended to file A's record: %q", rec)
		}
	}
}

func TestMultiline_MaxLinesEmitsTruncatedRatherThanGrowingForever(t *testing.T) {
	h := newMLHarness(t, MultilineOptions{StartPattern: tsPattern, MaxLines: 3})

	h.line("f1", "2026-08-17 start", 10)
	for i := 0; i < 50; i++ {
		h.line("f1", "  frame", int64(20+i))
	}

	got := h.messages()
	if len(got) != 1 {
		t.Fatalf("emitted %d records, want exactly 1 truncated one — the overflow must not become a stream of fragments", len(got))
	}
	if !strings.Contains(got[0], multilineTruncated) {
		t.Errorf("record was capped but not marked as truncated: %q", got[0])
	}

	// A new start begins a fresh record normally.
	h.line("f1", "2026-08-17 next", 200)
	h.a.flushAll()
	if len(h.messages()) != 2 {
		t.Errorf("a new start after an overflow should begin a fresh record, got %q", h.messages())
	}
}

func TestMultiline_ContinuationWithNoRecordIsStillEmitted(t *testing.T) {
	h := newMLHarness(t, MultilineOptions{StartPattern: tsPattern})

	// The file began mid-record, or the pattern does not match this format.
	// Either way the line must be visible — silence is how a wrong pattern goes
	// unnoticed.
	h.line("f1", "  orphaned line", 10)

	got := h.messages()
	if len(got) != 1 || got[0] != "  orphaned line" {
		t.Errorf("orphan handling = %q, want it emitted on its own", got)
	}
}

func TestMultiline_ForgetReleasesARotatedFile(t *testing.T) {
	h := newMLHarness(t, MultilineOptions{StartPattern: tsPattern})

	h.line("f1", "2026-08-17 pending", 10)
	h.a.forget("f1")

	if len(h.out) != 1 {
		t.Fatalf("rotating a file should release its pending record immediately, got %d", len(h.out))
	}
	if len(h.a.open) != 0 {
		t.Errorf("state for a closed file was retained: %d entries", len(h.a.open))
	}
}

func TestMultiline_RejectsAnUnusablePattern(t *testing.T) {
	if _, err := newMultilineAssembler(MultilineOptions{StartPattern: ""}, nil); err == nil {
		t.Error("an empty start pattern must be rejected, not silently treated as line-per-record")
	}
	if _, err := newMultilineAssembler(MultilineOptions{StartPattern: "([unclosed"}, nil); err == nil {
		t.Error("an invalid regular expression must be rejected at construction")
	}
}
