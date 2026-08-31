package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file collects the systemd journal, which on any modern Linux host is
// where the operating system's own logs live — sshd, the kernel, unit
// failures, authentication. They are not files, so the tailer in tail.go
// cannot reach them: journald keeps compressed binary files under
// /var/log/journal that it rotates and indexes itself.
//
// HOW THE COMMERCIAL AGENTS DO IT, AND WHY THIS DOES IT DIFFERENTLY
//
// Both Datadog and Dynatrace read the binary journal through libsystemd.
// Datadog links a cgo wrapper over the sd-journal C API
// (DataDog/go-systemd/sdjournal); Dynatrace dlopens libsystemd.so and maps the
// functions it needs. Either gives them cursor-based seeking and server-side
// matching for free.
//
// Neither is available here. This agent ships as a single static binary built
// with CGO_ENABLED=0 — see .github/workflows/release.yml and
// scripts/install.sh — and both approaches need cgo, dlopen included. Linking
// libsystemd would trade the property that makes get.sh work (drop one file on
// any Linux host, no runtime dependencies) for a nicer journal API.
//
// So this takes the third route, the same one the OpenTelemetry Collector's
// journaldreceiver takes: run journalctl as a subprocess and read its JSON
// output. The costs are real and worth stating — a child process per host, a
// JSON parse per entry, and a dependency on journalctl being installed — but
// none of them are the static binary.
//
// WHAT IS BORROWED
//
// The field mapping follows Dynatrace's, which is the sensible one:
// MESSAGE becomes the log body, PRIORITY becomes severity, and the unit name
// becomes the attribute you actually filter on — journald's equivalent of
// service.name.

// JournaldSource names where these envelopes entered the agent.
const JournaldSource = "journald"

// LabelJournalCursor carries the journal cursor of the entry an envelope came
// from. Underscore-prefixed like the tail labels beside it, for the same
// reason: it is plumbing that rides along with a signal and is stripped before
// the signal reaches a backend or a screen.
//
// It exists so the cursor is committed by whoever learns the entry is settled —
// the exporter — rather than by the reader. Committing on read is the bug the
// offset registry documents at length: an entry sitting in the export queue
// when the process dies would be recorded as handled and never read again.
const LabelJournalCursor = "_journal_cursor"

// JournaldOptions configures the reader.
type JournaldOptions struct {
	// Units limits collection to these systemd units. Empty means the whole
	// journal. Passed to journalctl, which does the filtering itself.
	Units []string
	// ExcludeUnits drops entries from these units. Filtered here rather than
	// by journalctl, which has no exclusion flag — the same split Datadog
	// makes between include_units and exclude_units.
	ExcludeUnits []string
	// Priority is a journalctl priority filter ("err", "warning", "4"), which
	// keeps everything at that level and more severe. Empty means everything.
	Priority string
	// Since seeds the first read on a host with no stored cursor. Empty means
	// start at the end of the journal, so enabling this does not replay
	// however many weeks of history the host happens to retain.
	Since string
	// JournalctlPath overrides the binary. The OTel collector needs the same
	// escape hatch: resolving from $PATH does not work inside a chroot.
	JournalctlPath string
	// CursorPath is where the last settled cursor is persisted. Empty disables
	// persistence, and a restart then resumes from Since.
	CursorPath string
}

// JournaldCollector streams the systemd journal into envelopes.
type JournaldCollector struct {
	agentID string
	opts    JournaldOptions
	cursors *CursorStore
	gate    *sendGate

	// newCmd builds the reader process. A field rather than a direct call to
	// exec.CommandContext so a test can substitute a script that prints known
	// entries, which is what lets the streaming, restart and cursor paths be
	// tested on a machine with no systemd at all.
	newCmd func(ctx context.Context, args []string) *exec.Cmd

	stop   context.CancelFunc
	done   chan struct{}
	closed sync.Once
}

func NewJournaldCollector(agentID string, opts JournaldOptions) *JournaldCollector {
	if opts.JournalctlPath == "" {
		opts.JournalctlPath = "journalctl"
	}
	return &JournaldCollector{
		agentID: agentID,
		opts:    opts,
		cursors: NewCursorStore(opts.CursorPath),
		gate:    newSendGate("journald"),
		newCmd: func(ctx context.Context, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, opts.JournalctlPath, args...)
		},
		done: make(chan struct{}),
	}
}

func (j *JournaldCollector) Name() string { return "logs.journald" }

// Cursors exposes the store the reader resumes from, so the daemon can commit
// into the same one as envelopes settle. Returning the collector's own store
// rather than having the caller build one is what stops the two from being
// different objects, where the reader would resume from a cursor nothing ever
// wrote to.
func (j *JournaldCollector) Cursors() *CursorStore { return j.cursors }

// Start launches the reader.
//
// A missing journalctl is a startup error rather than a warning. The whole
// point of this collector is that the operator asked for OS logs; carrying on
// without them would reproduce exactly the silent-partial-collection failure
// it exists to fix. It is off by default, so only a host that opted in can
// fail this way.
func (j *JournaldCollector) Start(ctx context.Context, out chan<- Envelope) error {
	if _, err := exec.LookPath(j.opts.JournalctlPath); err != nil {
		return fmt.Errorf("journald: %s not found (%w) — set logs.journald.enabled to false on a host without systemd, "+
			"or set journalctl_path", j.opts.JournalctlPath, err)
	}
	if err := j.cursors.Load(); err != nil {
		// Not fatal: an unreadable cursor file costs a replay window, and
		// refusing to start would cost every OS log on the host.
		log.Printf("journald: could not read cursor file %s (%v) — resuming from %s",
			j.opts.CursorPath, err, j.startDescription())
	}

	runCtx, cancel := context.WithCancel(ctx)
	j.stop = cancel
	go j.run(runCtx, out)
	return nil
}

func (j *JournaldCollector) startDescription() string {
	if j.opts.Since != "" {
		return "--since " + j.opts.Since
	}
	return "the end of the journal"
}

// run keeps a reader alive until the context is cancelled.
//
// journalctl --follow does not exit on its own, so an exit is a fault: the
// journal was rotated out from under it, systemd restarted, or it was killed.
// Restarting from the last committed cursor is what turns that from data loss
// into a gap of at most one backoff interval.
func (j *JournaldCollector) run(ctx context.Context, out chan<- Envelope) {
	defer close(j.done)
	defer func() {
		if err := j.cursors.Flush(); err != nil {
			log.Printf("journald: writing cursor on shutdown: %v", err)
		}
	}()

	// Flush on a timer as well as on shutdown, so a host that loses power does
	// not replay everything since it started.
	flush := time.NewTicker(10 * time.Second)
	defer flush.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-flush.C:
				if err := j.cursors.Flush(); err != nil {
					log.Printf("journald: writing cursor: %v", err)
				}
			}
		}
	}()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	// failures counts consecutive immediate failures, which is how a cursor
	// that can no longer be seeked to is recognised.
	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := j.stream(ctx, out)
		if ctx.Err() != nil {
			return
		}
		// A reader that survived a while was working; the next failure is a
		// new problem rather than a tight crash loop, so it starts over at the
		// short delay instead of inheriting a long one.
		if time.Since(started) > time.Minute {
			backoff, failures = time.Second, 0
		} else if err != nil {
			failures++
		}

		// A stored cursor is only valid while the entry it names is still in
		// the journal. journald vacuums by age and by size, so a cursor older
		// than the host's retention — an agent stopped over a weekend, a
		// journal rotated by a burst — cannot be seeked to and journalctl
		// exits immediately every single time ("Failed to seek to cursor").
		//
		// Without this the reader would retry that forever and collect
		// nothing: a permanent silent failure that looks exactly like a host
		// with no OS logs. Dropping the position loses the entries written
		// while it was invalid, which is the lesser harm and is said loudly
		// rather than swallowed.
		if failures >= cursorFailuresBeforeReset && j.cursors.Get() != "" {
			log.Printf("journald: reader failed %d times in a row with a stored cursor — "+
				"the journal has almost certainly rotated past it. Discarding the position and "+
				"restarting from %s; entries written in between are lost.",
				failures, j.startDescription())
			j.cursors.Reset()
			if err := j.cursors.Flush(); err != nil {
				log.Printf("journald: clearing cursor file: %v", err)
			}
			failures = 0
			backoff = time.Second
		}
		if err != nil {
			log.Printf("journald: reader stopped (%v) — restarting in %s", err, backoff)
		} else {
			log.Printf("journald: reader exited — restarting in %s", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// stream runs one journalctl process to completion.
func (j *JournaldCollector) stream(ctx context.Context, out chan<- Envelope) error {
	args := JournalctlArgs(j.opts, j.cursors.Get())
	cmd := j.newCmd(ctx, args)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// journalctl writes its own diagnostics to stderr; capturing a bounded
	// amount turns "exit status 1" into a message naming the cause, which is
	// usually a permissions problem on the journal directory.
	var stderr boundedBuffer
	cmd.Stderr = &stderr

	// Wait does not only wait for the process. Because Stderr here is not an
	// *os.File, os/exec runs its own goroutine copying from a pipe into the
	// buffer above, and Wait blocks until that copier finishes — which it
	// cannot while a surviving descendant still holds the write end. WaitDelay
	// is the only lever on that: past it, os/exec closes the pipes and returns.
	//
	// One second rather than the more generous value tried first. This is pure
	// shutdown latency in the pathological case, it is paid on every agent
	// stop, and on a healthy exit the pipes close on their own and none of it
	// is spent. Measured: without this, Stop took the full delay every time.
	cmd.WaitDelay = time.Second

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", j.opts.JournalctlPath, err)
	}

	// Closing the read end on cancellation is what actually unblocks consume,
	// and it is not optional. Killing the child does not guarantee the pipe
	// closes: a shell that forked rather than exec'd leaves the real process
	// holding the write end, so the read never returns EOF. Observed directly
	// — shutdown waited out its full timeout and every restart leaked the
	// previous reader's goroutine, each still blocked on a pipe nobody would
	// ever write to again.
	readDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = stdout.Close()
		case <-readDone:
		}
	}()

	j.consume(ctx, stdout, out)
	close(readDone)

	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// consume reads one JSON entry per line.
func (j *JournaldCollector) consume(ctx context.Context, r io.Reader, out chan<- Envelope) {
	scanner := bufio.NewScanner(r)
	// A journal entry can carry a large message — a stack trace, a kernel
	// dump. The default 64 KiB token limit would end the scan with an error
	// mid-stream and take the reader down with it.
	scanner.Buffer(make([]byte, 0, 64*1024), maxJournalLineBytes)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		env, ok := j.entryToEnvelope(line)
		if !ok {
			continue
		}
		j.gate.send(ctx, out, env)
	}
	// A read that ended because shutdown closed the pipe is not a fault, and
	// ctx.Err() is what distinguishes it from a genuine read failure.
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		log.Printf("journald: reading entries: %v", err)
	}
}

// cursorFailuresBeforeReset is how many consecutive immediate failures are
// tolerated before the stored position is treated as unusable. Two rather than
// one: a single failure is more likely to be a restarting journald or a
// momentary permissions problem, and discarding a good position costs data.
const cursorFailuresBeforeReset = 2

// maxJournalLineBytes caps one entry. Well above any real log line, and there
// to stop a malformed stream from being read into memory without limit.
const maxJournalLineBytes = 1 << 20

func (j *JournaldCollector) entryToEnvelope(line []byte) (Envelope, bool) {
	entry, err := ParseJournalEntry(line)
	if err != nil {
		return Envelope{}, false
	}
	if j.excluded(entry.Unit) {
		return Envelope{}, false
	}
	return entry.Envelope(j.agentID), true
}

func (j *JournaldCollector) excluded(unit string) bool {
	if unit == "" {
		return false
	}
	for _, e := range j.opts.ExcludeUnits {
		if e == unit {
			return true
		}
	}
	return false
}

func (j *JournaldCollector) Stop() error {
	j.closed.Do(func() {
		if j.stop != nil {
			j.stop()
		}
	})
	if j.done != nil {
		select {
		case <-j.done:
		case <-time.After(5 * time.Second):
			// The reader is blocked somewhere it should not be. Say so rather
			// than hanging shutdown indefinitely.
			log.Printf("journald: reader did not stop within 5s")
		}
	}
	return j.cursors.Flush()
}

// JournalctlArgs builds the command line.
//
// Exported so it can be tested directly: these flags decide whether a restart
// resumes, replays the whole journal, or silently skips what happened while
// the agent was down, and none of that is visible from the entries themselves.
func JournalctlArgs(o JournaldOptions, cursor string) []string {
	args := []string{"--output=json", "--no-pager", "--follow"}

	switch {
	case cursor != "":
		// after-cursor, not cursor: the stored entry was already exported, and
		// re-emitting it on every restart would duplicate a line each time.
		args = append(args, "--after-cursor", cursor)
	case o.Since != "":
		args = append(args, "--since", o.Since)
	default:
		// Without this journalctl --follow starts from the last 10 lines. On a
		// fresh install that is arbitrary: neither the whole history nor a
		// clean start. Ask for zero lines of history explicitly.
		args = append(args, "--lines", "0")
	}

	for _, u := range o.Units {
		if u = strings.TrimSpace(u); u != "" {
			args = append(args, "--unit", u)
		}
	}
	if p := strings.TrimSpace(o.Priority); p != "" {
		args = append(args, "--priority", p)
	}
	return args
}

// --- entry parsing ---

// JournalEntry is the subset of a journal record this agent uses.
type JournalEntry struct {
	Cursor    string
	Timestamp time.Time
	Message   string
	Priority  int
	// Unit is the systemd unit, from _SYSTEMD_UNIT, UNIT or the user-unit
	// equivalent. It is journald's answer to "which service wrote this" and is
	// the attribute worth filtering on.
	Unit       string
	Identifier string
	Hostname   string
	Transport  string
	PID        string
}

// ParseJournalEntry decodes one line of `journalctl --output=json`.
func ParseJournalEntry(line []byte) (JournalEntry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return JournalEntry{}, err
	}

	e := JournalEntry{
		Cursor:     journalField(raw, "__CURSOR"),
		Message:    journalField(raw, "MESSAGE"),
		Identifier: journalField(raw, "SYSLOG_IDENTIFIER"),
		Hostname:   journalField(raw, "_HOSTNAME"),
		Transport:  journalField(raw, "_TRANSPORT"),
		PID:        journalField(raw, "_PID"),
	}

	// _SYSTEMD_UNIT is the trusted field, stamped by journald itself; UNIT can
	// be supplied by the sender. Preferring the trusted one means an entry
	// cannot claim to come from a unit it did not. Dynatrace documents the
	// same pair, and the same gap when neither is present.
	for _, k := range []string{"_SYSTEMD_UNIT", "UNIT", "_SYSTEMD_USER_UNIT", "USER_UNIT"} {
		if v := journalField(raw, k); v != "" {
			e.Unit = v
			break
		}
	}

	// Priority is a syslog level 0-7. Absent means 6 (info), which is what
	// journald assumes for anything that did not set one.
	e.Priority = 6
	if p := journalField(raw, "PRIORITY"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 && n <= 7 {
			e.Priority = n
		}
	}

	// Microseconds since the epoch, as a string. Falling back to now keeps an
	// entry inside the retention window rather than placing it in 1970, where
	// no view would ever show it.
	e.Timestamp = time.Now().UTC()
	if ts := journalField(raw, "__REALTIME_TIMESTAMP"); ts != "" {
		if usec, err := strconv.ParseInt(ts, 10, 64); err == nil && usec > 0 {
			e.Timestamp = time.Unix(usec/1e6, (usec%1e6)*1e3).UTC()
		}
	}

	return e, nil
}

// journalField reads one field, coping with all three shapes journalctl emits.
//
// A field is normally a JSON string. It is an array of byte values when the
// content is not valid UTF-8 — journald stores arbitrary bytes and the JSON
// output has no other way to express them — and an array of strings when the
// same field appeared more than once in the entry. Treating any of those as a
// plain string silently loses the field, which for MESSAGE means an empty log
// line where there was content.
func journalField(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok || len(v) == 0 {
		return ""
	}
	switch v[0] {
	case '"':
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s
		}
	case '[':
		// Try bytes first: a non-UTF-8 value is an array of numbers.
		var nums []byte
		if err := json.Unmarshal(v, &nums); err == nil {
			return string(nums)
		}
		var strs []string
		if err := json.Unmarshal(v, &strs); err == nil {
			// A repeated field. The last occurrence is the one journald would
			// show, and joining them would invent a value that was never sent.
			for i := len(strs) - 1; i >= 0; i-- {
				if strs[i] != "" {
					return strs[i]
				}
			}
		}
	default:
		// A bare number, which some fields use.
		return strings.Trim(string(v), `"`)
	}
	return ""
}

// Envelope renders the entry in the agent's own shape.
func (e JournalEntry) Envelope(agentID string) Envelope {
	severityText, severityNumber := journalSeverity(e.Priority)

	labels := map[string]string{
		"severity":        severityText,
		"severity.number": strconv.Itoa(severityNumber),
		"priority":        strconv.Itoa(e.Priority),
	}
	// Only what the entry actually carried. An empty label is worse than an
	// absent one: it renders as a column of blanks that looks like data.
	set := func(k, v string) {
		if v != "" {
			labels[k] = v
		}
	}
	set("unit", e.Unit)
	set("syslog.identifier", e.Identifier)
	set("host.name", e.Hostname)
	set("transport", e.Transport)
	set("pid", e.PID)
	set(LabelJournalCursor, e.Cursor)

	return Envelope{
		Kind:      KindLog,
		AgentID:   agentID,
		Source:    JournaldSource,
		Timestamp: e.Timestamp,
		Labels:    labels,
		Message:   e.Message,
	}
}

// journalSeverity maps a syslog priority onto a severity name and an OTel
// severity number.
//
// The numbers follow OpenTelemetry's syslog mapping rather than being invented
// here, so they line up with the severity.number the OTLP log receiver already
// sets from an SDK — otherwise a filter for "error and above" would mean two
// different things depending on which collector produced the line.
func journalSeverity(priority int) (string, int) {
	switch priority {
	case 0:
		return "EMERG", 24
	case 1:
		return "ALERT", 23
	case 2:
		return "CRIT", 22
	case 3:
		return "ERROR", 17
	case 4:
		return "WARN", 13
	case 5:
		return "NOTICE", 10
	case 7:
		return "DEBUG", 5
	default:
		return "INFO", 9
	}
}

// --- cursor persistence ---

// CursorStore holds the journal cursor of the last entry known to have been
// exported.
//
// Written by whoever retires an envelope, which is the exporter's sender
// goroutine, and read by the reader when it starts a journalctl. Both can
// happen at once, hence the mutex.
type CursorStore struct {
	path  string
	mu    sync.Mutex
	value string
	dirty bool
}

func NewCursorStore(path string) *CursorStore { return &CursorStore{path: path} }

// Load reads the persisted cursor. A missing file is not an error: it is what a
// first run looks like.
func (c *CursorStore) Load() error {
	if c == nil || c.path == "" {
		return nil
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = strings.TrimSpace(string(b))
	return nil
}

func (c *CursorStore) Get() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

// Set records a cursor. Ignores an empty one so a malformed entry cannot erase
// a good position and cause a replay.
func (c *CursorStore) Set(cursor string) {
	if c == nil || cursor == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != cursor {
		c.value, c.dirty = cursor, true
	}
}

// Reset forgets the stored position, so the next reader starts from Since or
// the end of the journal instead of from a cursor that cannot be seeked to.
func (c *CursorStore) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != "" {
		c.value, c.dirty = "", true
	}
}

// Flush persists the cursor, via a temporary file and a rename so a crash
// mid-write cannot leave a truncated cursor that resumes from nowhere. Same
// idiom as the offset registry.
func (c *CursorStore) Flush() error {
	if c == nil || c.path == "" {
		return nil
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	value := c.value
	c.dirty = false
	c.mu.Unlock()

	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing journal cursor: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("replacing journal cursor: %w", err)
	}
	return nil
}

// CommitJournalCursor records that an envelope has been exported, so a restart
// does not re-read the entry it came from. The journald counterpart to
// CommitTailOffset, and called from the same place for the same reason.
func CommitJournalCursor(store *CursorStore, e Envelope) {
	if store == nil || len(e.Labels) == 0 {
		return
	}
	store.Set(e.Labels[LabelJournalCursor])
}

// boundedBuffer collects at most a fixed amount of a child process's stderr.
// Unbounded would mean a process that fails by printing forever takes the
// agent's memory with it.
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const maxStderrBytes = 4 << 10

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxStderrBytes - len(b.buf); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf = append(b.buf, p...)
	}
	// Reports the full length: the caller wrote it, we simply chose not to
	// keep all of it, and a short write would be treated as an I/O error.
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
