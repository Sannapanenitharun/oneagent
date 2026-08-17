package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// collectLines runs a tail manager over path until it has seen want lines or
// the deadline passes, and reports what it saw. commit selects whether lines
// are treated as accounted for — which is the whole point of these tests.
func collectLines(t *testing.T, path, regPath string, commit bool, want int) []string {
	t.Helper()

	reg, err := NewOffsetRegistry(regPath)
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}

	got := make(chan string, 64)
	m := newTailManager(tailOptions{
		globs:        []string{path},
		scanInterval: 50 * time.Millisecond,
		pollInterval: 10 * time.Millisecond,
		maxLineBytes: 1024,
		registry:     reg,
		handle: func(ln tailLine) {
			if commit {
				reg.Commit(ln.FileID, ln.Path, ln.EndOffset, ln.Fingerprint)
			}
			got <- ln.Line
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)

	var lines []string
	deadline := time.After(2 * time.Second)
loop:
	for len(lines) < want {
		select {
		case l := <-got:
			lines = append(lines, l)
		case <-deadline:
			break loop
		}
	}
	// Let anything already in flight land, so a test asserting "nothing more
	// arrives" is not just winning a race.
	time.Sleep(120 * time.Millisecond)
	for {
		select {
		case l := <-got:
			lines = append(lines, l)
			continue
		default:
		}
		break
	}

	cancel()
	m.Stop()
	if err := reg.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return lines
}

// seedRegistry creates an entry for path at offset zero, standing in for a file
// the agent has already been following. Without an entry, startup deliberately
// seeks to the end of a pre-existing file, which would mask what these tests
// are checking.
func seedRegistry(t *testing.T, path, regPath string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	reg, err := NewOffsetRegistry(regPath)
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	reg.Commit(FileID(fi), path, 0, 0)
	if err := reg.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// The gap this change closes: a line that was read, handed downstream, and then
// lost when the process died must be read again. Committing on read recorded it
// as handled and it was gone for good.
func TestTail_UnretiredLinesAreReadAgainAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	regPath := filepath.Join(dir, "reg.json")

	writeFile(t, path, "one\ntwo\nthree\n")
	seedRegistry(t, path, regPath)

	first := collectLines(t, path, regPath, false, 3)
	if len(first) != 3 {
		t.Fatalf("first run read %d lines (%v), want 3", len(first), first)
	}

	// Nothing was accounted for, so a restart must see all of it again.
	second := collectLines(t, path, regPath, false, 3)
	if len(second) != 3 {
		t.Errorf("after a restart with nothing accounted for, re-read %d lines (%v), want all 3",
			len(second), second)
	}
}

// The other half of the contract: lines that WERE accounted for must not be
// re-sent, or every restart would duplicate the tail of every log file.
func TestTail_RetiredLinesAreNotReadAgain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	regPath := filepath.Join(dir, "reg.json")

	writeFile(t, path, "one\ntwo\nthree\n")
	seedRegistry(t, path, regPath)

	first := collectLines(t, path, regPath, true, 3)
	if len(first) != 3 {
		t.Fatalf("first run read %d lines (%v), want 3", len(first), first)
	}

	second := collectLines(t, path, regPath, true, 0)
	if len(second) != 0 {
		t.Errorf("re-read %v after those lines were already accounted for", second)
	}
}

// Blank lines and other skipped content are never emitted, so they can never be
// committed on their own. They must still not wedge the offset: the next real
// line carries a position past them.
func TestTail_SkippedContentDoesNotWedgeTheOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse.log")
	regPath := filepath.Join(dir, "reg.json")

	writeFile(t, path, "\n\n\nreal line\n\n\n")
	seedRegistry(t, path, regPath)

	first := collectLines(t, path, regPath, true, 1)
	if len(first) != 1 || first[0] != "real line" {
		t.Fatalf("first run read %v, want exactly [real line]", first)
	}

	second := collectLines(t, path, regPath, true, 0)
	if len(second) != 0 {
		t.Errorf("re-read %v — the blank lines around the real one blocked the commit", second)
	}
}
