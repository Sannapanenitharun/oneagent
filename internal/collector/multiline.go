package collector

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

// Defaults for multiline assembly. Both are backstops rather than tuning knobs:
// a correct start pattern makes neither of them fire.
const (
	defaultMultilineMaxLines = 500
	defaultMultilineTimeout  = 2 * time.Second
	// multilineJoin separates the physical lines of one record. A newline keeps
	// a stack trace looking like a stack trace wherever it is eventually read.
	multilineJoin = "\n"
	// multilineTruncated marks a record cut short by MaxLines, so a consumer can
	// tell a capped record from one that genuinely ended there.
	multilineTruncated = "...RECORD TRUNCATED..."
)

// MultilineOptions configures the assembler. Zero values take the defaults.
type MultilineOptions struct {
	StartPattern string
	MaxLines     int
	Timeout      time.Duration
}

// multilineAssembler joins continuation lines onto the record they belong to.
//
// The rule is deliberately one-directional: a line matching StartPattern begins
// a new record, and everything else continues the current one. A record is
// emitted when the next one starts, when it hits MaxLines, or when it has been
// idle for Timeout — not when it is "complete", because nothing in a log file
// says a record is complete.
//
// State is per file, because two tailed files interleave freely and a stack
// trace from one must not swallow a line from the other.
//
// Every method runs on the tail manager's single goroutine — handle() from the
// read path and flushIdle() from the poll tick — so there are no locks here.
type multilineAssembler struct {
	start    *regexp.Regexp
	maxLines int
	timeout  time.Duration
	emit     func(tailLine)

	open map[string]*openRecord
	now  func() time.Time // injectable for tests
}

// openRecord is a record still accumulating lines.
type openRecord struct {
	// first carries the identity and timestamp of the line that started the
	// record: a stack trace belongs to the moment it began, not the moment its
	// last frame was written.
	first tailLine
	lines []string
	// end is the offset just past the most recent line, so committing the
	// assembled record accounts for every byte it consumed.
	end       int64
	fp        uint64
	lastSeen  time.Time
	truncated bool
	// dropping is set once a record has been emitted at MaxLines; the rest of
	// its lines are discarded until a new record starts, so one runaway record
	// cannot produce an endless stream of truncated fragments.
	dropping bool
}

// newMultilineAssembler compiles the start pattern. A pattern that does not
// compile is a configuration error the caller must surface — silently falling
// back to line-per-record would look like the feature was working.
func newMultilineAssembler(opts MultilineOptions, emit func(tailLine)) (*multilineAssembler, error) {
	if strings.TrimSpace(opts.StartPattern) == "" {
		return nil, fmt.Errorf("logs.multiline.enabled is true but start_pattern is empty — it must be a regular expression matching the first line of a record, e.g. `^\\d{4}-\\d{2}-\\d{2}`")
	}
	re, err := regexp.Compile(opts.StartPattern)
	if err != nil {
		return nil, fmt.Errorf("logs.multiline.start_pattern %q is not a valid regular expression: %w", opts.StartPattern, err)
	}
	if opts.MaxLines <= 0 {
		opts.MaxLines = defaultMultilineMaxLines
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultMultilineTimeout
	}
	return &multilineAssembler{
		start:    re,
		maxLines: opts.MaxLines,
		timeout:  opts.Timeout,
		emit:     emit,
		open:     map[string]*openRecord{},
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// handle takes one physical line and either starts a record, extends one, or
// emits the previous one first.
func (a *multilineAssembler) handle(ln tailLine) {
	rec := a.open[ln.FileID]
	isStart := a.start.MatchString(ln.Line)

	if isStart {
		// A new record begins: whatever was open is finished by definition.
		a.flush(ln.FileID)
		a.open[ln.FileID] = &openRecord{
			first:    ln,
			lines:    []string{ln.Line},
			end:      ln.EndOffset,
			fp:       ln.Fingerprint,
			lastSeen: a.now(),
		}
		return
	}

	if rec == nil {
		// A continuation with nothing to continue: the file started mid-record,
		// or the pattern does not match this format. Emit it on its own rather
		// than dropping it — a line the operator can see is how they find out
		// the pattern is wrong.
		a.emit(ln)
		return
	}

	// Keep the offset moving even while discarding, so a runaway record does
	// not wedge the file's commit position.
	rec.end = ln.EndOffset
	if ln.Fingerprint != 0 {
		rec.fp = ln.Fingerprint
	}
	rec.lastSeen = a.now()

	if rec.dropping {
		return
	}
	if len(rec.lines) >= a.maxLines {
		rec.truncated = true
		rec.dropping = true
		log.Printf("tail: a record in %s reached %d lines without a new one starting — emitting it truncated. "+
			"If this repeats, logs.multiline.start_pattern is probably not matching this file's format.",
			ln.Path, a.maxLines)
		a.flushRecordKeepingSlot(ln.FileID, rec)
		return
	}
	rec.lines = append(rec.lines, ln.Line)
}

// flushIdle emits records that have waited longer than the timeout. Called from
// the tail manager's poll tick, which is what makes the last record in a quiet
// file appear instead of being held forever.
func (a *multilineAssembler) flushIdle() {
	if len(a.open) == 0 {
		return
	}
	cutoff := a.now().Add(-a.timeout)
	for id, rec := range a.open {
		if rec.lastSeen.Before(cutoff) {
			a.flush(id)
		}
	}
}

// flushAll emits everything still open. Called at shutdown so an unfinished
// record is reported rather than discarded.
func (a *multilineAssembler) flushAll() {
	for id := range a.open {
		a.flush(id)
	}
}

// flush emits the open record for a file, if any, and clears the slot.
func (a *multilineAssembler) flush(fileID string) {
	rec, ok := a.open[fileID]
	if !ok {
		return
	}
	delete(a.open, fileID)
	a.emitRecord(rec)
}

// flushRecordKeepingSlot emits a capped record but leaves the slot in place, so
// the remaining lines are recognised as belonging to it and discarded rather
// than each becoming a record of its own.
func (a *multilineAssembler) flushRecordKeepingSlot(fileID string, rec *openRecord) {
	a.emitRecord(rec)
	// Retain identity and the dropping flag; drop the accumulated text.
	rec.lines = nil
}

func (a *multilineAssembler) emitRecord(rec *openRecord) {
	if rec == nil || len(rec.lines) == 0 {
		return
	}
	text := strings.Join(rec.lines, multilineJoin)
	if rec.truncated {
		text += multilineJoin + multilineTruncated
	}
	out := rec.first
	out.Line = text
	// The record's position is where its LAST line ended, so committing it
	// accounts for every byte it absorbed.
	out.EndOffset = rec.end
	out.Fingerprint = rec.fp
	a.emit(out)
}

// forget drops any state for a file that is no longer being tailed, after
// emitting what it had. Without this, a host that rotates many files would
// accumulate one map entry per file for the life of the process.
func (a *multilineAssembler) forget(fileID string) {
	a.flush(fileID)
}
