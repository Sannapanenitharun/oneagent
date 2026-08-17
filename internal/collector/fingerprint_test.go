package collector

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func openFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestFileFingerprint_NoneUntilTheFileIsLongEnough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.log")
	writeFile(t, path, strings.Repeat("a", fingerprintBytes-1))

	if _, ok := fileFingerprint(openFile(t, path)); ok {
		t.Error("a file shorter than the fingerprint window should not produce one")
	}

	writeFile(t, path, strings.Repeat("a", fingerprintBytes))
	if _, ok := fileFingerprint(openFile(t, path)); !ok {
		t.Error("a file of exactly the window size should produce a fingerprint")
	}
}

// The property the whole scheme rests on: the head of an append-only file never
// changes, so its fingerprint is stable for the file's whole life.
func TestFileFingerprint_StableAsTheFileGrows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grow.log")
	head := strings.Repeat("h", fingerprintBytes)
	writeFile(t, path, head)

	first, ok := fileFingerprint(openFile(t, path))
	if !ok {
		t.Fatal("expected a fingerprint")
	}

	for i := 0; i < 5; i++ {
		writeFile(t, path, head+strings.Repeat("tail", 500*(i+1)))
		got, ok := fileFingerprint(openFile(t, path))
		if !ok {
			t.Fatalf("append %d: expected a fingerprint", i)
		}
		if got != first {
			t.Fatalf("append %d: fingerprint changed %d -> %d as the file grew", i, first, got)
		}
	}
}

func TestFileFingerprint_DiffersWhenTheHeadDiffers(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	writeFile(t, a, strings.Repeat("a", fingerprintBytes))
	writeFile(t, b, strings.Repeat("b", fingerprintBytes))

	fa, _ := fileFingerprint(openFile(t, a))
	fb, _ := fileFingerprint(openFile(t, b))
	if fa == fb {
		t.Error("different content produced the same fingerprint")
	}

	// A change beyond the window is invisible, which is intended: it is the
	// head that identifies the file, and appends must not change identity.
	writeFile(t, a, strings.Repeat("a", fingerprintBytes)+"completely different")
	fa2, _ := fileFingerprint(openFile(t, a))
	if fa2 != fa {
		t.Error("a change past the fingerprint window should not change identity")
	}
}

func TestFileFingerprint_DoesNotDisturbTheReadPosition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seek.log")
	writeFile(t, path, strings.Repeat("z", fingerprintBytes*2))

	f := openFile(t, path)
	if _, err := f.Seek(1500, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, ok := fileFingerprint(f); !ok {
		t.Fatal("expected a fingerprint")
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("tell: %v", err)
	}
	if pos != 1500 {
		t.Errorf("fingerprinting moved the file position to %d, want 1500", pos)
	}
}

// ---- registry semantics ----

func TestRegistry_CommitOnlyEverAdvances(t *testing.T) {
	reg, err := NewOffsetRegistry(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}

	reg.Commit("dev:1", "/a.log", 500, 99)
	// A stale report from the other goroutine must not rewind the offset, or a
	// restart would re-send lines that were already accounted for.
	reg.Commit("dev:1", "/a.log", 100, 99)

	got, fp, ok := reg.Lookup("dev:1")
	if !ok {
		t.Fatal("entry missing")
	}
	if got != 500 {
		t.Errorf("offset = %d after a stale commit, want 500", got)
	}
	if fp != 99 {
		t.Errorf("fingerprint = %d, want 99", fp)
	}

	reg.Commit("dev:1", "/a.log", 900, 99)
	if got, _, _ := reg.Lookup("dev:1"); got != 900 {
		t.Errorf("offset = %d, want 900", got)
	}
}

func TestRegistry_CommitDoesNotErasureAKnownFingerprint(t *testing.T) {
	reg, err := NewOffsetRegistry(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}

	reg.Commit("dev:2", "/b.log", 100, 4242)
	// Lines emitted before the file was long enough to fingerprint carry zero.
	// Letting those erase a known value would give up inode-reuse protection.
	reg.Commit("dev:2", "/b.log", 200, 0)

	_, fp, _ := reg.Lookup("dev:2")
	if fp != 4242 {
		t.Errorf("fingerprint = %d after a zero commit, want 4242 preserved", fp)
	}
}

func TestRegistry_ResetClearsBothOffsetAndFingerprint(t *testing.T) {
	reg, err := NewOffsetRegistry(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}

	reg.Commit("dev:3", "/c.log", 800, 7777)
	reg.Reset("dev:3", "/c.log")

	offset, fp, ok := reg.Lookup("dev:3")
	if !ok {
		t.Fatal("entry should still exist after Reset")
	}
	if offset != 0 {
		t.Errorf("offset = %d after Reset, want 0", offset)
	}
	if fp != 0 {
		t.Errorf("fingerprint = %d after Reset, want 0 — a truncated file holds different content", fp)
	}
}

func TestRegistry_SurvivesAReloadFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.json")
	reg, err := NewOffsetRegistry(path)
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	reg.Commit("dev:4", "/d.log", 1234, 5678)
	if err := reg.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reloaded, err := NewOffsetRegistry(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	offset, fp, ok := reloaded.Lookup("dev:4")
	if !ok {
		t.Fatal("entry did not survive the reload")
	}
	if offset != 1234 || fp != 5678 {
		t.Errorf("reloaded offset=%d fp=%d, want 1234/5678", offset, fp)
	}
}

// ---- provenance labels ----

func TestCommitTailOffset_AdvancesFromEnvelopeLabels(t *testing.T) {
	reg, err := NewOffsetRegistry(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}

	env := Envelope{
		Kind:   KindLog,
		Source: "/var/log/app.log",
		Labels: tailProvenanceLabels(tailLine{
			Path:        "/var/log/app.log",
			FileID:      "dev:9",
			EndOffset:   4096,
			Fingerprint: 31337,
		}),
	}
	CommitTailOffset(reg, env)

	offset, fp, ok := reg.Lookup("dev:9")
	if !ok {
		t.Fatal("no entry was recorded")
	}
	if offset != 4096 {
		t.Errorf("offset = %d, want 4096", offset)
	}
	if fp != 31337 {
		t.Errorf("fingerprint = %d, want 31337", fp)
	}
}

func TestCommitTailOffset_IgnoresEnvelopesWithNoFileBehindThem(t *testing.T) {
	reg, err := NewOffsetRegistry(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}

	// A host metric, a span, anything not tailed from a file.
	for _, env := range []Envelope{
		{Kind: KindMetric, Source: "host.cpu.used_pct"},
		{Kind: KindTrace, Source: "otlp.span", Labels: map[string]string{"service.name": "x"}},
		{Kind: KindLog, Source: "/a.log", Labels: map[string]string{LabelTailID: "dev:1"}}, // no offset
	} {
		CommitTailOffset(reg, env) // must not panic
	}
	if _, _, ok := reg.Lookup("dev:1"); ok {
		t.Error("an envelope with no usable offset should not create a registry entry")
	}
}

func TestTailProvenanceLabels_OmitsAnUnknownFingerprint(t *testing.T) {
	l := tailProvenanceLabels(tailLine{FileID: "dev:1", EndOffset: 10})
	if _, present := l[LabelTailFP]; present {
		t.Error("a zero fingerprint should be omitted, not encoded as \"0\"")
	}
	if l[LabelTailEnd] != strconv.Itoa(10) {
		t.Errorf("end offset = %q, want \"10\"", l[LabelTailEnd])
	}
	if tailProvenanceLabels(tailLine{}) != nil {
		t.Error("a line with no file id should produce no labels at all")
	}
}

// ---- the bug this exists to prevent ----

// An inode can be recycled: delete a log file and the next file created can be
// handed the same number. Resuming on the stored offset then skips the new
// file's opening content. Seeding the registry with the real file id but a
// foreign fingerprint reproduces that exactly, without needing the filesystem
// to actually recycle an inode.
func TestTailOpen_RecycledInodeIsReadFromTheStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recycled.log")
	writeFile(t, path, strings.Repeat("n", fingerprintBytes)+"\nnew content\n")

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	reg, err := NewOffsetRegistry(filepath.Join(dir, "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	// The previous occupant of this inode was read up to byte 900 and looked
	// nothing like this file.
	reg.Commit(FileID(fi), path, 900, 0xBADF00D)

	m := newTailManager(tailOptions{
		globs:        []string{path},
		maxLineBytes: 1024,
		registry:     reg,
		handle:       func(tailLine) {},
	})
	tl, err := m.open(path, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tl.close()

	if tl.offset != 0 {
		t.Errorf("resumed at offset %d on a recycled inode — the new file's opening lines would be skipped; want 0", tl.offset)
	}
}

func TestTailOpen_MatchingFingerprintResumesWhereItLeftOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same.log")
	writeFile(t, path, strings.Repeat("s", fingerprintBytes)+"\nmore\n")

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	fp, ok := fileFingerprint(openFile(t, path))
	if !ok {
		t.Fatal("expected a fingerprint")
	}
	reg, err := NewOffsetRegistry(filepath.Join(dir, "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	reg.Commit(FileID(fi), path, 900, fp)

	m := newTailManager(tailOptions{
		globs:        []string{path},
		maxLineBytes: 1024,
		registry:     reg,
		handle:       func(tailLine) {},
	})
	tl, err := m.open(path, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tl.close()

	if tl.offset != 900 {
		t.Errorf("offset = %d, want 900 — a genuine resume was treated as a new file", tl.offset)
	}
}

func TestTailOpen_ShrunkFileIsReadFromTheStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shrunk.log")
	writeFile(t, path, strings.Repeat("l", fingerprintBytes*2))

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	reg, err := NewOffsetRegistry(filepath.Join(dir, "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	reg.Commit(FileID(fi), path, 1500, 0xABCD)

	// Rewritten much smaller: it cannot be the content we fingerprinted.
	writeFile(t, path, "tiny\n")

	m := newTailManager(tailOptions{
		globs:        []string{path},
		maxLineBytes: 1024,
		registry:     reg,
		handle:       func(tailLine) {},
	})
	tl, err := m.open(path, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tl.close()

	if tl.offset != 0 {
		t.Errorf("offset = %d on a file that shrank below its fingerprint window, want 0", tl.offset)
	}
}
