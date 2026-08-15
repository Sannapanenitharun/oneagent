package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests cover the four failure modes the old per-file goroutine tailer
// had, each of which was silent in production: rotation, files appearing after
// startup, partial lines, and losing your place across a restart. They drive
// tailManager directly rather than going through a collector, so a failure
// points at the tailing logic rather than at envelope construction.

func startTestTail(t *testing.T, ctx context.Context, globs []string, regPath string) (<-chan string, *tailManager) {
	t.Helper()
	reg, err := NewOffsetRegistry(regPath)
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	lines := make(chan string, 128)
	m := newTailManager(tailOptions{
		globs:        globs,
		scanInterval: 150 * time.Millisecond,
		pollInterval: 25 * time.Millisecond,
		maxLineBytes: 1024,
		registry:     reg,
		handle: func(_, line string, _ time.Time) {
			lines <- line
		},
	})
	m.Start(ctx)
	// Give the initial scan time to open what already exists, so tests can
	// append immediately afterwards without racing startup.
	time.Sleep(150 * time.Millisecond)
	return lines, m
}

func tailWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func tailAppend(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("appending to %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("appending to %s: %v", path, err)
	}
}

func expectTailLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got := <-lines:
		if got != want {
			t.Fatalf("got line %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for line %q", want)
	}
}

func expectNoTailLine(t *testing.T, lines <-chan string, within time.Duration) {
	t.Helper()
	select {
	case got := <-lines:
		t.Fatalf("expected no line yet, got %q", got)
	case <-time.After(within):
	}
}

// TestTail_FollowsFileAcrossRotation is the headline case: logrotate renames
// the file and creates a new one at the same path. The old code kept reading
// the renamed inode forever and reported nothing further, with no error.
func TestTail_FollowsFileAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	tailWrite(t, path, "pre-existing\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, m := startTestTail(t, ctx, []string{path}, filepath.Join(dir, "reg.json"))
	defer m.Stop()

	tailAppend(t, path, "before-rotation\n")
	expectTailLine(t, lines, "before-rotation")

	// Rotate the way logrotate's default (non-copytruncate) mode does.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotating: %v", err)
	}
	tailWrite(t, path, "after-rotation\n")

	expectTailLine(t, lines, "after-rotation")
}

// TestTail_HandlesInPlaceTruncation covers logrotate's copytruncate mode,
// where the inode stays the same but the file is emptied underneath us.
func TestTail_HandlesInPlaceTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	tailWrite(t, path, "pre-existing\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, m := startTestTail(t, ctx, []string{path}, filepath.Join(dir, "reg.json"))
	defer m.Stop()

	tailAppend(t, path, "before-truncate\n")
	expectTailLine(t, lines, "before-truncate")

	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncating: %v", err)
	}
	tailAppend(t, path, "after-truncate\n")

	expectTailLine(t, lines, "after-truncate")
}

// TestTail_PicksUpFileCreatedAfterStartup: the old code evaluated the glob
// exactly once, so a log file created later was never tailed at all.
func TestTail_PicksUpFileCreatedAfterStartup(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, m := startTestTail(t, ctx, []string{filepath.Join(dir, "*.log")}, filepath.Join(dir, "reg.json"))
	defer m.Stop()

	// A file that did not exist at startup is genuinely new, so it is read
	// from the beginning rather than from EOF.
	tailWrite(t, filepath.Join(dir, "late.log"), "first-line\n")

	expectTailLine(t, lines, "first-line")
}

// TestTail_DoesNotEmitPartialLines: reading mid-write previously produced the
// first half as a complete line and the second half as another one.
func TestTail_DoesNotEmitPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	tailWrite(t, path, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, m := startTestTail(t, ctx, []string{path}, filepath.Join(dir, "reg.json"))
	defer m.Stop()

	tailAppend(t, path, "this line is not finished")
	expectNoTailLine(t, lines, 300*time.Millisecond)

	tailAppend(t, path, " yet\n")
	expectTailLine(t, lines, "this line is not finished yet")
}

// TestTail_ResumesFromRegistryAfterRestart proves the offset registry does its
// job: lines written while the agent is down are delivered on the next start,
// and lines already delivered are not repeated. Previously the tailer always
// seeked to EOF, so everything written during a restart was lost silently.
func TestTail_ResumesFromRegistryAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	regPath := filepath.Join(dir, "reg.json")
	tailWrite(t, path, "pre-existing\n")

	ctx1, cancel1 := context.WithCancel(context.Background())
	lines1, m1 := startTestTail(t, ctx1, []string{path}, regPath)
	tailAppend(t, path, "during-first-run\n")
	expectTailLine(t, lines1, "during-first-run")
	m1.Stop()
	cancel1()

	// Written while nothing is tailing.
	tailAppend(t, path, "while-agent-was-down\n")

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	lines2, m2 := startTestTail(t, ctx2, []string{path}, regPath)
	defer m2.Stop()

	// Exactly the missed line: not skipped, and not a replay of the file.
	expectTailLine(t, lines2, "while-agent-was-down")
	expectNoTailLine(t, lines2, 300*time.Millisecond)
}

// TestTail_UpdateGlobsPicksUpNewPath covers config reload: adding a path must
// start tailing it without restarting the agent, and from the beginning of the
// file rather than from EOF — someone adding a path wants its contents.
func TestTail_UpdateGlobsPicksUpNewPath(t *testing.T) {
	dir := t.TempDir()
	watched := filepath.Join(dir, "watched.log")
	other := filepath.Join(dir, "other.log")
	tailWrite(t, watched, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, m := startTestTail(t, ctx, []string{watched}, filepath.Join(dir, "reg.json"))
	defer m.Stop()

	// Not matched by the current glob, so nothing should arrive.
	tailWrite(t, other, "from-unwatched-file\n")
	expectNoTailLine(t, lines, 300*time.Millisecond)

	m.UpdateGlobs([]string{watched, other})

	expectTailLine(t, lines, "from-unwatched-file")
}

// TestTail_UpdateGlobsDropsRemovedPath: a path removed from config stops being
// tailed.
func TestTail_UpdateGlobsDropsRemovedPath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	tailWrite(t, a, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, m := startTestTail(t, ctx, []string{a}, filepath.Join(dir, "reg.json"))
	defer m.Stop()

	tailAppend(t, a, "while-watched\n")
	expectTailLine(t, lines, "while-watched")

	m.UpdateGlobs([]string{filepath.Join(dir, "nothing-matches-*.log")})
	time.Sleep(200 * time.Millisecond)

	tailAppend(t, a, "after-removal\n")
	expectNoTailLine(t, lines, 400*time.Millisecond)
}

// TestTail_TruncatesOverlongLine guards the memory bound: a file with no
// newlines must not buffer without limit.
func TestTail_TruncatesOverlongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	tailWrite(t, path, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, m := startTestTail(t, ctx, []string{path}, filepath.Join(dir, "reg.json"))
	defer m.Stop()

	long := make([]byte, 4096) // maxLineBytes in these tests is 1024
	for i := range long {
		long[i] = 'x'
	}
	tailAppend(t, path, string(long)+"\n")
	tailAppend(t, path, "next-line\n")

	select {
	case got := <-lines:
		if len(got) != 1024+len(truncatedSuffix) {
			t.Fatalf("truncated line length = %d, want %d", len(got), 1024+len(truncatedSuffix))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for truncated line")
	}

	// The remainder of the over-long line must be discarded, not delivered as
	// a bogus second line.
	expectTailLine(t, lines, "next-line")
}
