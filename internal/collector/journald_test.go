package collector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- entry parsing ---

// The ordinary case: what journalctl emits for a line written by a unit.
func TestParseJournalEntry_Basic(t *testing.T) {
	line := []byte(`{"__CURSOR":"s=abc;i=1","__REALTIME_TIMESTAMP":"1786800000000000",
	  "PRIORITY":"3","_SYSTEMD_UNIT":"sshd.service","SYSLOG_IDENTIFIER":"sshd",
	  "_HOSTNAME":"web-01","_TRANSPORT":"journal","_PID":"920",
	  "MESSAGE":"Failed password for root"}`)

	e, err := ParseJournalEntry(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Message != "Failed password for root" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Unit != "sshd.service" {
		t.Errorf("unit = %q", e.Unit)
	}
	if e.Priority != 3 {
		t.Errorf("priority = %d", e.Priority)
	}
	if e.Cursor != "s=abc;i=1" {
		t.Errorf("cursor = %q", e.Cursor)
	}
	// __REALTIME_TIMESTAMP is microseconds, not milliseconds or seconds. Off
	// by a factor of 1000 puts every OS log outside the retention window.
	if got := e.Timestamp.UnixNano(); got != 1786800000000000000 {
		t.Errorf("timestamp = %d, want 1786800000000000000", got)
	}
	if e.Identifier != "sshd" || e.Hostname != "web-01" || e.PID != "920" {
		t.Errorf("identity fields = %q / %q / %q", e.Identifier, e.Hostname, e.PID)
	}
}

// journald stores arbitrary bytes, and journalctl renders a field that is not
// valid UTF-8 as an array of byte values. Read as a string it decodes to
// nothing, which for MESSAGE means an empty log line where there was content.
func TestParseJournalEntry_ByteArrayMessage(t *testing.T) {
	// [104 105] is "hi".
	line := []byte(`{"__CURSOR":"c","MESSAGE":[104,105],"PRIORITY":"6"}`)

	e, err := ParseJournalEntry(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Message != "hi" {
		t.Errorf("message = %q, want %q — a byte-array field was dropped", e.Message, "hi")
	}
}

// A field that appeared more than once in an entry arrives as an array of
// strings. The last one is what journald itself would show.
func TestParseJournalEntry_RepeatedField(t *testing.T) {
	line := []byte(`{"__CURSOR":"c","MESSAGE":"m","_SYSTEMD_UNIT":["first.service","second.service"]}`)

	e, err := ParseJournalEntry(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Unit != "second.service" {
		t.Errorf("unit = %q, want the last occurrence", e.Unit)
	}
}

// _SYSTEMD_UNIT is stamped by journald; UNIT can come from the sender. The
// trusted one has to win, or an entry could claim a unit it did not come from.
func TestParseJournalEntry_PrefersTrustedUnitField(t *testing.T) {
	line := []byte(`{"__CURSOR":"c","MESSAGE":"m","UNIT":"claimed.service","_SYSTEMD_UNIT":"real.service"}`)

	e, err := ParseJournalEntry(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Unit != "real.service" {
		t.Errorf("unit = %q, want real.service", e.Unit)
	}
}

// An entry with no PRIORITY is info, which is what journald assumes. An entry
// with no timestamp must land in the retention window rather than in 1970.
func TestParseJournalEntry_Defaults(t *testing.T) {
	e, err := ParseJournalEntry([]byte(`{"MESSAGE":"bare"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Priority != 6 {
		t.Errorf("priority = %d, want 6", e.Priority)
	}
	if age := time.Since(e.Timestamp); age < 0 || age > time.Minute {
		t.Errorf("timestamp %v is not close to now", e.Timestamp)
	}
}

func TestParseJournalEntry_RejectsGarbage(t *testing.T) {
	if _, err := ParseJournalEntry([]byte("not json at all")); err == nil {
		t.Error("want an error for a non-JSON line")
	}
}

// The severity numbers have to agree with the ones the OTLP log receiver sets
// from an SDK, or "error and above" means two different things depending on
// which collector produced the line.
func TestJournalSeverity_MatchesOTelScale(t *testing.T) {
	cases := []struct {
		priority int
		text     string
		number   int
	}{
		{0, "EMERG", 24},
		{1, "ALERT", 23},
		{2, "CRIT", 22},
		{3, "ERROR", 17},
		{4, "WARN", 13},
		{5, "NOTICE", 10},
		{6, "INFO", 9},
		{7, "DEBUG", 5},
	}
	for _, c := range cases {
		text, num := journalSeverity(c.priority)
		if text != c.text || num != c.number {
			t.Errorf("priority %d = %q/%d, want %q/%d", c.priority, text, num, c.text, c.number)
		}
	}
}

// The envelope is what the rest of the agent sees, so the mapping from a
// journal entry into it is the contract that matters.
func TestJournalEntry_Envelope(t *testing.T) {
	e := JournalEntry{
		Cursor: "s=abc;i=9", Timestamp: time.Unix(1786800000, 0).UTC(),
		Message: "out of memory", Priority: 2, Unit: "app.service",
		Identifier: "kernel", Hostname: "web-01", Transport: "kernel", PID: "1",
	}
	env := e.Envelope("agent-1")

	if env.Kind != KindLog {
		t.Errorf("kind = %q", env.Kind)
	}
	if env.Source != JournaldSource {
		t.Errorf("source = %q", env.Source)
	}
	if env.Message != "out of memory" {
		t.Errorf("message = %q", env.Message)
	}
	if env.Labels["unit"] != "app.service" {
		t.Errorf("unit label = %q", env.Labels["unit"])
	}
	if env.Labels["severity"] != "CRIT" || env.Labels["severity.number"] != "22" {
		t.Errorf("severity = %q / %q", env.Labels["severity"], env.Labels["severity.number"])
	}
	// The cursor rides along as private plumbing and must be underscore
	// prefixed, so it is stripped before reaching a backend or a screen.
	if env.Labels[LabelJournalCursor] != "s=abc;i=9" {
		t.Errorf("cursor label = %q", env.Labels[LabelJournalCursor])
	}
	if !strings.HasPrefix(LabelJournalCursor, "_") {
		t.Errorf("cursor label %q must be underscore-prefixed to be stripped", LabelJournalCursor)
	}
}

// An absent field must be absent, not present and empty — a blank column looks
// like data that was collected and found to be nothing.
func TestJournalEntry_OmitsAbsentFields(t *testing.T) {
	env := JournalEntry{Message: "m", Priority: 6}.Envelope("agent-1")
	for _, k := range []string{"unit", "syslog.identifier", "host.name", "transport", "pid"} {
		if _, present := env.Labels[k]; present {
			t.Errorf("label %q present for an entry that carried none", k)
		}
	}
}

// --- command line ---

// These flags decide whether a restart resumes, replays the journal, or skips
// what happened while the agent was down — none of which is visible from the
// entries themselves.
func TestJournalctlArgs(t *testing.T) {
	t.Run("cursor wins over since", func(t *testing.T) {
		args := JournalctlArgs(JournaldOptions{Since: "-1h"}, "s=abc;i=1")
		if !hasPair(args, "--after-cursor", "s=abc;i=1") {
			t.Errorf("args = %v, want --after-cursor", args)
		}
		// after-cursor, not cursor: the stored entry was already exported.
		if contains(args, "--cursor") {
			t.Errorf("args = %v, must not re-emit the stored entry", args)
		}
		if contains(args, "--since") {
			t.Errorf("args = %v, --since would override the resume point", args)
		}
	})

	t.Run("since seeds a first run", func(t *testing.T) {
		args := JournalctlArgs(JournaldOptions{Since: "-1h"}, "")
		if !hasPair(args, "--since", "-1h") {
			t.Errorf("args = %v", args)
		}
	})

	t.Run("no cursor and no since starts at the end", func(t *testing.T) {
		args := JournalctlArgs(JournaldOptions{}, "")
		// Without --lines 0, journalctl --follow replays the last 10 entries:
		// neither the whole history nor a clean start.
		if !hasPair(args, "--lines", "0") {
			t.Errorf("args = %v, want --lines 0", args)
		}
	})

	t.Run("units and priority", func(t *testing.T) {
		args := JournalctlArgs(JournaldOptions{
			Units:    []string{"sshd.service", " ", "cron.service"},
			Priority: "warning",
		}, "")
		if !hasPair(args, "--unit", "sshd.service") || !hasPair(args, "--unit", "cron.service") {
			t.Errorf("args = %v, want both units", args)
		}
		if hasPair(args, "--unit", " ") || hasPair(args, "--unit", "") {
			t.Errorf("args = %v, a blank unit entry became a filter matching nothing", args)
		}
		if !hasPair(args, "--priority", "warning") {
			t.Errorf("args = %v", args)
		}
	})

	t.Run("always JSON and following", func(t *testing.T) {
		args := JournalctlArgs(JournaldOptions{}, "")
		for _, want := range []string{"--output=json", "--follow", "--no-pager"} {
			if !contains(args, want) {
				t.Errorf("args = %v, missing %s", args, want)
			}
		}
	})
}

// --- cursor persistence ---

func TestCursorStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journald.cursor")
	s := NewCursorStore(path)

	s.Set("s=abc;i=1")
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	reloaded := NewCursorStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reloaded.Get() != "s=abc;i=1" {
		t.Errorf("cursor = %q after reload", reloaded.Get())
	}
}

// A first run has no cursor file, which is not an error.
func TestCursorStore_MissingFileIsNotAnError(t *testing.T) {
	s := NewCursorStore(filepath.Join(t.TempDir(), "absent.cursor"))
	if err := s.Load(); err != nil {
		t.Errorf("load of a missing cursor file: %v", err)
	}
	if s.Get() != "" {
		t.Errorf("cursor = %q, want empty", s.Get())
	}
}

// An entry without a cursor must not erase a good position — that would replay
// everything since the last flush.
func TestCursorStore_EmptySetIsIgnored(t *testing.T) {
	s := NewCursorStore(filepath.Join(t.TempDir(), "c"))
	s.Set("s=good;i=1")
	s.Set("")
	if s.Get() != "s=good;i=1" {
		t.Errorf("cursor = %q, an empty Set erased it", s.Get())
	}
}

// Disabled persistence must be inert rather than a crash or a stray file.
func TestCursorStore_NoPathIsInert(t *testing.T) {
	s := NewCursorStore("")
	s.Set("x")
	if err := s.Flush(); err != nil {
		t.Errorf("flush with no path: %v", err)
	}
	if err := s.Load(); err != nil {
		t.Errorf("load with no path: %v", err)
	}
}

// The cursor is committed by whoever learns the entry was exported, which is
// the exporter — the same contract CommitTailOffset has.
func TestCommitJournalCursor(t *testing.T) {
	s := NewCursorStore(filepath.Join(t.TempDir(), "c"))
	CommitJournalCursor(s, Envelope{Labels: map[string]string{LabelJournalCursor: "s=abc;i=7"}})
	if s.Get() != "s=abc;i=7" {
		t.Errorf("cursor = %q", s.Get())
	}

	// Anything that did not come from the journal carries no cursor and must
	// be ignored rather than clearing the position.
	CommitJournalCursor(s, Envelope{Labels: map[string]string{"host": "x"}})
	CommitJournalCursor(s, Envelope{})
	if s.Get() != "s=abc;i=7" {
		t.Errorf("cursor = %q after unrelated envelopes", s.Get())
	}
}

// --- streaming, with a stand-in for journalctl ---

// fakeJournalctl replaces the reader process with a shell script, so the
// streaming, exclusion, restart and resume paths can be tested on a machine
// with no systemd. It records the arguments of every invocation, which is how
// the resume behaviour is observed.
type fakeJournalctl struct {
	mu     sync.Mutex
	script string
	calls  [][]string
}

func (f *fakeJournalctl) install(j *JournaldCollector) {
	// LookPath in Start must succeed, and sh is what actually runs.
	j.opts.JournalctlPath = "sh"
	j.newCmd = func(ctx context.Context, args []string) *exec.Cmd {
		f.mu.Lock()
		f.calls = append(f.calls, args)
		f.mu.Unlock()
		return exec.CommandContext(ctx, "sh", "-c", f.script)
	}
}

func (f *fakeJournalctl) invocations() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func recvEnvelope(t *testing.T, out chan Envelope) Envelope {
	t.Helper()
	select {
	case e := <-out:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an envelope")
		return Envelope{}
	}
}

// The whole path: a process emitting JSON lines becomes envelopes the rest of
// the agent can carry.
func TestJournaldCollector_StreamsEntries(t *testing.T) {
	fake := &fakeJournalctl{script: `
	  echo '{"__CURSOR":"s=a;i=1","__REALTIME_TIMESTAMP":"1786800000000000","PRIORITY":"3","_SYSTEMD_UNIT":"sshd.service","MESSAGE":"first"}'
	  echo '{"__CURSOR":"s=a;i=2","__REALTIME_TIMESTAMP":"1786800001000000","PRIORITY":"6","_SYSTEMD_UNIT":"cron.service","MESSAGE":"second"}'
	  sleep 30`}

	j := NewJournaldCollector("agent-1", JournaldOptions{})
	fake.install(j)

	out := make(chan Envelope, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := j.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer j.Stop()

	first := recvEnvelope(t, out)
	if first.Message != "first" || first.Labels["unit"] != "sshd.service" {
		t.Errorf("first = %q from %q", first.Message, first.Labels["unit"])
	}
	if first.Labels["severity"] != "ERROR" {
		t.Errorf("severity = %q", first.Labels["severity"])
	}
	if first.Kind != KindLog || first.AgentID != "agent-1" {
		t.Errorf("kind/agent = %q / %q", first.Kind, first.AgentID)
	}

	second := recvEnvelope(t, out)
	if second.Message != "second" || second.Labels["severity"] != "INFO" {
		t.Errorf("second = %q / %q", second.Message, second.Labels["severity"])
	}
}

// A malformed line in the middle of a stream must not end collection — the
// journal is a shared bus and one bad producer should not silence the rest.
func TestJournaldCollector_SkipsGarbageAndKeepsReading(t *testing.T) {
	fake := &fakeJournalctl{script: `
	  echo 'this is not json'
	  echo '{"__CURSOR":"s=a;i=2","MESSAGE":"survived"}'
	  sleep 30`}

	j := NewJournaldCollector("agent-1", JournaldOptions{})
	fake.install(j)

	out := make(chan Envelope, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := j.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer j.Stop()

	if e := recvEnvelope(t, out); e.Message != "survived" {
		t.Errorf("message = %q", e.Message)
	}
}

// journalctl has no exclusion flag, so exclude_units is enforced here. A unit
// that is excluded must produce nothing at all.
func TestJournaldCollector_ExcludesUnits(t *testing.T) {
	fake := &fakeJournalctl{script: `
	  echo '{"__CURSOR":"s=a;i=1","_SYSTEMD_UNIT":"noisy.service","MESSAGE":"dropped"}'
	  echo '{"__CURSOR":"s=a;i=2","_SYSTEMD_UNIT":"sshd.service","MESSAGE":"kept"}'
	  sleep 30`}

	j := NewJournaldCollector("agent-1", JournaldOptions{ExcludeUnits: []string{"noisy.service"}})
	fake.install(j)

	out := make(chan Envelope, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := j.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer j.Stop()

	e := recvEnvelope(t, out)
	if e.Message != "kept" {
		t.Errorf("message = %q, want the excluded unit to have been dropped", e.Message)
	}
}

// journalctl --follow does not exit on its own, so an exit is a fault. The
// reader has to come back, and it has to come back from where it left off —
// otherwise every restart either loses entries or replays them.
func TestJournaldCollector_RestartsAndResumesFromCursor(t *testing.T) {
	fake := &fakeJournalctl{script: `
	  echo '{"__CURSOR":"s=a;i=1","MESSAGE":"before the exit"}'`} // exits immediately

	j := NewJournaldCollector("agent-1", JournaldOptions{})
	fake.install(j)

	out := make(chan Envelope, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := j.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer j.Stop()

	// Stand in for the exporter settling the envelope, which is what commits
	// the cursor in the running agent.
	first := recvEnvelope(t, out)
	CommitJournalCursor(j.Cursors(), first)

	// The reader restarts after its backoff and must ask to resume.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		calls := fake.invocations()
		if len(calls) >= 2 {
			if !hasPair(calls[1], "--after-cursor", "s=a;i=1") {
				t.Fatalf("restart args = %v, want --after-cursor s=a;i=1", calls[1])
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("reader did not restart; invocations = %v", fake.invocations())
}

// A stored cursor must reach the very first invocation, or an agent restart
// replays or skips depending on the Since setting.
func TestJournaldCollector_UsesStoredCursorOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journald.cursor")
	if err := os.WriteFile(path, []byte("s=stored;i=42\n"), 0o644); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	fake := &fakeJournalctl{script: "sleep 30"}
	j := NewJournaldCollector("agent-1", JournaldOptions{CursorPath: path, Since: "-1h"})
	fake.install(j)

	out := make(chan Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := j.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer j.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if calls := fake.invocations(); len(calls) > 0 {
			if !hasPair(calls[0], "--after-cursor", "s=stored;i=42") {
				t.Fatalf("first invocation = %v, want the stored cursor", calls[0])
			}
			// Shut down inside the test rather than in a defer, so the time
			// it takes is attributed to this test and a reader that will not
			// stop shows up as a slow test instead of a silent log line.
			stopped := time.Now()
			if err := j.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if elapsed := time.Since(stopped); elapsed > 2*time.Second {
				t.Errorf("Stop took %s", elapsed)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("reader never started")
}

// journald vacuums old entries, so a stored cursor outlives the entry it names
// whenever the agent was down longer than the host's retention. journalctl then
// exits immediately every time with "Failed to seek to cursor", and a reader
// that kept retrying it would collect nothing for as long as the host lived —
// a permanent silent failure indistinguishable from a host with no OS logs.
func TestJournaldCollector_DiscardsUnseekableCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journald.cursor")
	if err := os.WriteFile(path, []byte("s=gone;i=1\n"), 0o644); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	// Stands in for journalctl refusing the cursor: writes the same message to
	// stderr and exits non-zero, immediately, every time.
	fake := &fakeJournalctl{script: `echo "Failed to seek to cursor: Invalid argument" >&2; exit 1`}
	j := NewJournaldCollector("agent-1", JournaldOptions{CursorPath: path})
	fake.install(j)

	out := make(chan Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := j.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer j.Stop()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		calls := fake.invocations()
		// The first invocations carry the doomed cursor; the recovery is a
		// later one that carries none.
		for i, args := range calls {
			if i > 0 && !contains(args, "--after-cursor") {
				if j.Cursors().Get() != "" {
					t.Errorf("cursor %q still stored after the reset", j.Cursors().Get())
				}
				// And the bad position must be gone from disk too, or the next
				// agent restart walks straight back into the same loop.
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading cursor file: %v", err)
				}
				if strings.Contains(string(b), "s=gone") {
					t.Errorf("cursor file still holds the unusable position: %q", b)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("cursor was never discarded; invocations = %v", fake.invocations())
}

// Enabling this on a host without systemd must fail loudly at startup. Carrying
// on would reproduce the silent partial collection it exists to fix.
func TestJournaldCollector_MissingBinaryIsAStartupError(t *testing.T) {
	j := NewJournaldCollector("agent-1", JournaldOptions{
		JournalctlPath: "definitely-not-a-real-binary-name-9182",
	})
	err := j.Start(context.Background(), make(chan Envelope, 1))
	if err == nil {
		t.Fatal("want an error when journalctl is absent")
	}
	if !strings.Contains(err.Error(), "journald") {
		t.Errorf("error %q should name the collector so the operator knows what to disable", err)
	}
}

// Stop must return promptly rather than hanging shutdown on a reader that is
// blocked in a --follow that never ends.
func TestJournaldCollector_StopIsPrompt(t *testing.T) {
	fake := &fakeJournalctl{script: "sleep 60"}
	j := NewJournaldCollector("agent-1", JournaldOptions{})
	fake.install(j)

	out := make(chan Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := j.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Deliberately tighter than the collector's own 5s give-up timeout. An
	// assertion looser than that passes while Stop waits out the full timeout
	// and logs that the reader never stopped — which is exactly what happened
	// here, and what hid a reader blocked forever on a pipe that a killed
	// shell's surviving child still held open.
	done := make(chan struct{})
	start := time.Now()
	go func() { _ = j.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return promptly — the reader is stuck")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Stop took %s; the reader is not being unblocked on cancellation", elapsed)
	}

	// And calling it twice must not panic on an already-closed channel.
	if err := j.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

// --- helpers ---

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// hasPair reports whether want appears immediately followed by value, so a
// flag is checked together with the argument it takes.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
