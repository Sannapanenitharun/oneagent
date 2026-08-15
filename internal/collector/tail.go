package collector

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// This file implements rotation-aware file tailing, shared by the plain log
// tailer (logs.go) and the HTTP access log tailer (accesslog.go).
//
// Both previously globbed once at startup, opened each match, seeked to EOF
// and read from that descriptor forever. Three things went wrong with that,
// and all three were silent — no error, no log line, just missing telemetry:
//
//  1. Rotation. logrotate renames the file and creates a new one at the old
//     path. Our descriptor stays valid and stays empty forever, so the agent
//     tails a deleted inode until someone restarts it.
//  2. Late files. A path matching the glob that appeared after startup was
//     never picked up at all, because the glob was only ever evaluated once.
//  3. Partial lines. bufio.Reader.ReadString returns whatever it has when it
//     hits EOF mid-line, so a line caught halfway through a write was emitted
//     as a complete line and its remainder was emitted as a second one.
//
// The shape here — a manager that rescans on an interval owning one tailer
// per file, plus a persistent offset registry — is the design mature agents
// converge on. Reading happens through raw file reads rather than bufio so
// that the byte offset we persist is exactly the offset we have emitted, with
// no hidden buffering in between.

// lineFunc receives one complete line (newline and any trailing CR removed).
type lineFunc func(path, line string, at time.Time)

// tailOptions configures a tailManager.
type tailOptions struct {
	globs        []string
	scanInterval time.Duration // how often to re-evaluate the globs
	pollInterval time.Duration // how often to read from open files
	maxLineBytes int           // safety cap on a single line
	registry     *OffsetRegistry
	handle       lineFunc
}

// TailingOptions carries the tunables shared by every file-tailing collector.
// The daemon builds one of these from config and hands the same value (and the
// same registry) to both the log and access-log collectors, so the two never
// write competing offset files.
type TailingOptions struct {
	ScanInterval time.Duration
	PollInterval time.Duration
	MaxLineBytes int
	Registry     *OffsetRegistry
}

// truncatedSuffix marks a line cut short by maxLineBytes, so a consumer can
// tell a truncated line from a genuinely long one.
const truncatedSuffix = "...TRUNCATED..."

type tailManager struct {
	opts    tailOptions
	tailers map[string]*fileTailer
	stop    chan struct{}
	done    chan struct{}
	// globsCh delivers a replacement glob set on config reload. Buffered so
	// the sender never blocks; see UpdateGlobs.
	globsCh chan []string
	// emptyGlobs remembers which globs matched nothing, so the warning is
	// stated once rather than repeated every scan for the life of the process.
	// Cleared for a glob as soon as it matches, so a file that appears later
	// warns again if it disappears.
	emptyGlobs map[string]bool
}

func newTailManager(opts tailOptions) *tailManager {
	if opts.scanInterval <= 0 {
		opts.scanInterval = 30 * time.Second
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = 500 * time.Millisecond
	}
	if opts.maxLineBytes <= 0 {
		opts.maxLineBytes = 256 * 1024
	}
	return &tailManager{
		opts:       opts,
		tailers:    map[string]*fileTailer{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		globsCh:    make(chan []string, 1),
		emptyGlobs: map[string]bool{},
	}
}

// UpdateGlobs replaces the watched glob set, taking effect immediately rather
// than at the next scan.
//
// The send is non-blocking and deliberately so. The caller is the daemon's
// drain loop, and this manager may at that moment be blocked sending an
// envelope to that same loop — a blocking send here would deadlock the two
// against each other. A superseded pending update is discarded, so the newest
// glob set always wins.
func (m *tailManager) UpdateGlobs(globs []string) {
	select {
	case <-m.globsCh:
	default:
	}
	select {
	case m.globsCh <- globs:
	default:
	}
}

// Start runs the scan/poll loop in one goroutine. Everything the manager owns
// is touched only from that goroutine, so no locking is needed anywhere in
// this file; the registry has its own lock because it is shared between the
// log and access-log managers.
func (m *tailManager) Start(ctx context.Context) {
	go m.run(ctx)
}

// Stop signals the loop and waits for it to finish, so open descriptors and
// the final offset flush are done before the caller proceeds.
func (m *tailManager) Stop() {
	select {
	case <-m.stop:
		// already stopped
	default:
		close(m.stop)
	}
	<-m.done
}

func (m *tailManager) run(ctx context.Context) {
	defer close(m.done)

	// The first scan is special: files already present at startup are opened
	// at EOF (unless the registry knows better), because replaying whatever
	// history happens to be on disk would flood the exporter on every boot.
	// Files that appear later are genuinely new and are read from the start.
	m.scan(ctx, true)

	pollTicker := time.NewTicker(m.opts.pollInterval)
	defer pollTicker.Stop()
	scanTicker := time.NewTicker(m.opts.scanInterval)
	defer scanTicker.Stop()
	flushTicker := time.NewTicker(5 * time.Second)
	defer flushTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-m.stop:
			m.shutdown()
			return
		case <-pollTicker.C:
			m.poll(ctx)
		case <-scanTicker.C:
			m.scan(ctx, false)
		case globs := <-m.globsCh:
			// Files matching a newly added glob are new to us, so scan with
			// first=false: they are read from the beginning rather than from
			// EOF, which is what someone adding a path expects.
			m.opts.globs = globs
			// Forget which globs were empty, so a reload re-reports the state
			// of the new set. Someone who just edited logs.paths to fix exactly
			// this needs to see whether the new path matched, not silence left
			// over from the old one.
			m.emptyGlobs = map[string]bool{}
			log.Printf("tail: watching %d glob(s) after config reload", len(globs))
			m.scan(ctx, false)
		case <-flushTicker.C:
			if err := m.opts.registry.Flush(); err != nil {
				log.Printf("tail: flushing offset registry: %v", err)
			}
		}
	}
}

func (m *tailManager) shutdown() {
	for path, t := range m.tailers {
		t.close()
		delete(m.tailers, path)
	}
	if err := m.opts.registry.Flush(); err != nil {
		log.Printf("tail: flushing offset registry on shutdown: %v", err)
	}
}

// scan re-evaluates the globs, opening tailers for new matches and dropping
// tailers whose path no longer matches.
func (m *tailManager) scan(ctx context.Context, first bool) {
	matched := map[string]bool{}
	for _, g := range m.opts.globs {
		found, err := filepath.Glob(g)
		if err != nil {
			log.Printf("tail: bad glob %q: %v", g, err)
			continue
		}
		if len(found) == 0 {
			// Say so. A glob matching nothing used to be silent, which made a
			// misconfigured path indistinguishable from a quiet log file — the
			// agent looked healthy, logs.enabled was true, and no line ever
			// arrived with nothing anywhere explaining why. The default config
			// ships /var/log/app/*.log, a path most hosts do not have, so this
			// is the first thing a new install gets wrong.
			if !m.emptyGlobs[g] {
				log.Printf("tail: WARNING %q matches no files — nothing is being collected from it", g)
				m.emptyGlobs[g] = true
			}
			continue
		}
		// Matching again after being empty is worth stating too: it is the
		// confirmation that a config fix took effect.
		if m.emptyGlobs[g] {
			log.Printf("tail: %q now matches %d file(s)", g, len(found))
			delete(m.emptyGlobs, g)
		}
		for _, p := range found {
			matched[p] = true
		}
	}

	for p := range matched {
		if _, ok := m.tailers[p]; ok {
			continue
		}
		t, err := m.open(p, first)
		if err != nil {
			// A file we cannot open yet (permissions, transient) is retried on
			// the next scan rather than being abandoned for the process
			// lifetime, which is what the old code did.
			log.Printf("tail: cannot open %s: %v", p, err)
			continue
		}
		m.tailers[p] = t
		log.Printf("tail: following %s from offset %d", p, t.offset)
	}

	for p, t := range m.tailers {
		if !matched[p] {
			// The path stopped matching (deleted, or rotated to a name outside
			// the glob). Drain what is left on the descriptor before dropping.
			t.readAvailable(ctx)
			t.close()
			delete(m.tailers, p)
			log.Printf("tail: stopped following %s", p)
		}
	}
}

func (m *tailManager) poll(ctx context.Context) {
	for path, t := range m.tailers {
		t.readAvailable(ctx)
		if !t.rotated() {
			continue
		}
		// Rotation detected. readAvailable has already drained the old
		// descriptor to EOF, so anything still unread is genuinely lost and
		// worth saying so out loud.
		t.reportMissed()
		t.close()
		delete(m.tailers, path)

		// Reopen immediately at the start of the new file rather than waiting
		// for the next scan, so the rotation gap stays sub-second.
		if nt, err := m.open(path, false); err == nil {
			m.tailers[path] = nt
			log.Printf("tail: %s rotated, following new file", path)
		}
	}
}

// open starts tailing path. atStartup selects the behaviour for a file with no
// registry entry: at startup we skip existing content, later we read it all.
func (m *tailManager) open(path string, atStartup bool) (*fileTailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	t := &fileTailer{
		path:    path,
		f:       f,
		id:      FileID(fi),
		maxLine: m.opts.maxLineBytes,
		reg:     m.opts.registry,
		handle:  m.opts.handle,
		buf:     make([]byte, 32*1024),
	}

	offset, known := m.reg().Get(t.id)
	switch {
	case known && offset <= fi.Size():
		// Resume where the previous run left off.
	case known:
		// The recorded offset is past the end: the file was truncated in place
		// while we were down. Start over rather than seeking past EOF.
		offset = 0
	case atStartup:
		offset = fi.Size()
	default:
		offset = 0
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	t.offset = offset
	return t, nil
}

func (m *tailManager) reg() *OffsetRegistry { return m.opts.registry }

// fileTailer follows a single open file.
type fileTailer struct {
	path    string
	f       *os.File
	id      string
	offset  int64
	maxLine int
	reg     *OffsetRegistry
	handle  lineFunc

	buf     []byte
	partial []byte
	// skipToNewline is set after emitting a truncated line, so the remainder
	// of that over-long line is discarded instead of arriving as a bogus
	// second line.
	skipToNewline bool
	// missed is the number of bytes known to have been left unread on this
	// descriptor, recorded when rotation is detected.
	missed int64
}

// readAvailable drains the file to EOF, emitting every complete line and
// holding back any trailing partial line for the next call.
func (t *fileTailer) readAvailable(ctx context.Context) {
	for {
		n, err := t.f.Read(t.buf)
		if n > 0 {
			t.offset += int64(n)
			t.consume(ctx, t.buf[:n])
			t.reg.Set(t.id, t.path, t.offset)
		}
		if err != nil || n == 0 {
			// io.EOF is the normal case: we have caught up and will look again
			// on the next tick.
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (t *fileTailer) consume(ctx context.Context, b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			t.partial = append(t.partial, b...)
			if len(t.partial) > t.maxLine {
				t.emit(ctx, string(t.partial[:t.maxLine])+truncatedSuffix)
				t.partial = t.partial[:0]
				t.skipToNewline = true
			}
			return
		}

		// Build the string before reslicing, since t.partial and full share a
		// backing array.
		full := append(t.partial, b[:i]...)
		line := string(bytes.TrimRight(full, "\r"))
		// Reuse the grown buffer rather than the original backing array.
		t.partial = full[:0]
		b = b[i+1:]

		if t.skipToNewline {
			t.skipToNewline = false
			continue
		}
		// The cap applies here too, not just to lines accumulated across
		// reads: a single over-long line that happens to arrive complete in
		// one read would otherwise be emitted at full length.
		if len(line) > t.maxLine {
			line = line[:t.maxLine] + truncatedSuffix
		}
		if line != "" {
			t.emit(ctx, line)
		}
	}
}

func (t *fileTailer) emit(ctx context.Context, line string) {
	// handle ultimately sends on the daemon's envelope channel, which can
	// block under backpressure; ctx makes that interruptible so shutdown never
	// waits on a stalled exporter.
	select {
	case <-ctx.Done():
		return
	default:
	}
	t.handle(t.path, line, time.Now().UTC())
}

// rotated reports whether the file we hold open is no longer the file at our
// path, or was truncated underneath us. Truncation is handled here directly
// (seek back to zero) because it does not require reopening.
func (t *fileTailer) rotated() bool {
	onDisk, err := os.Stat(t.path)
	if err != nil {
		// Path is gone: renamed away by logrotate, or deleted. Either way
		// nothing further will arrive on this descriptor.
		return true
	}
	open, err := t.f.Stat()
	if err != nil {
		return true
	}
	if !os.SameFile(onDisk, open) {
		// Same path, different inode: rename + create.
		return true
	}
	if onDisk.Size() < t.offset {
		// Same inode, smaller than our offset: truncated in place, which is
		// what logrotate's copytruncate mode does.
		if _, err := t.f.Seek(0, io.SeekStart); err == nil {
			t.offset = 0
			t.partial = t.partial[:0]
			t.skipToNewline = false
			t.reg.Set(t.id, t.path, 0)
			log.Printf("tail: %s was truncated, restarting from the beginning", t.path)
		}
		return false
	}
	return false
}

// reportMissed logs any bytes left unread on a rotated-away descriptor. In
// normal operation this is zero, because poll drains to EOF before checking
// for rotation; a non-zero value means the agent could not keep up, which is
// exactly the kind of loss that should not be silent.
func (t *fileTailer) reportMissed() {
	fi, err := t.f.Stat()
	if err != nil {
		return
	}
	if remaining := fi.Size() - t.offset; remaining > 0 {
		t.missed = remaining
		log.Printf("tail: %s rotated with %d bytes unread — those lines are lost", t.path, remaining)
	}
}

func (t *fileTailer) close() {
	if len(t.partial) > 0 {
		// A file that ends without a trailing newline still has a last line.
		t.handle(t.path, string(bytes.TrimRight(t.partial, "\r")), time.Now().UTC())
		t.partial = t.partial[:0]
	}
	t.reg.Set(t.id, t.path, t.offset)
	t.f.Close()
}
