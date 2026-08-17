package collector

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// OffsetRegistry persists how far into each tailed file the agent has read, so
// a restart resumes at the right place. Without it there are only two bad
// options: seek to end on every start (silently losing everything written
// while the agent was down) or seek to start (re-sending the entire file).
//
// Entries are keyed by device+inode, NOT by path. That distinction is the
// whole point of the file: logrotate renames a file and creates a new one at
// the old path, so a path-keyed offset gets applied to the wrong file
// immediately after every rotation. The inode follows the bytes.
type OffsetRegistry struct {
	path string

	mu      sync.Mutex
	entries map[string]*registryEntry
	dirty   bool
}

type registryEntry struct {
	// Path is the last path this inode was seen at. Recorded for debugging
	// only — never used for lookup, for the reason described above.
	Path    string    `json:"path"`
	Offset  int64     `json:"offset"`
	Updated time.Time `json:"updated"`
	// Fingerprint is a checksum of the file's first bytes, and it is what makes
	// the inode key trustworthy. Inodes get recycled, so dev+inode can match a
	// file that has nothing to do with the one whose offset we stored; the
	// fingerprint catches that. Zero means "not known" — either the file was
	// too short to fingerprint, or the entry predates this field — and callers
	// must treat zero as "no opinion" rather than as a mismatch, so upgrading
	// the agent does not re-send every log file it was already tracking.
	Fingerprint uint64 `json:"fp,omitempty"`
}

// registryMaxAge bounds how long an entry for a file we no longer see is kept.
// Rotated-away files would otherwise accumulate forever on a busy host.
const registryMaxAge = 7 * 24 * time.Hour

// NewOffsetRegistry loads the registry at path, creating its directory if
// needed.
//
// Nothing here is a startup error. Losing read offsets costs some duplicate or
// skipped lines once; refusing to boot costs all telemetry from the host until
// someone notices. That applies to an unwritable directory too — an agent
// binary upgraded in place without re-running the installer will not have
// /var/lib/agent-i yet, and ProtectSystem=strict will block creating
// it. In that case the registry degrades to in-memory only: offsets stay
// consistent for the life of the process and simply do not survive a restart.
func NewOffsetRegistry(path string) (*OffsetRegistry, error) {
	if path == "" {
		return nil, fmt.Errorf("offset registry: path is required")
	}

	r := &OffsetRegistry{path: path, entries: map[string]*registryEntry{}}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("offset registry: cannot create %s (%v) — offsets will not survive a restart; "+
			"re-run the installer to create the directory", filepath.Dir(path), err)
		r.path = "" // disables Flush
		return r, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("offset registry: cannot read %s (%v) — starting with empty offsets", path, err)
		}
		return r, nil
	}
	if err := json.Unmarshal(raw, &r.entries); err != nil {
		log.Printf("offset registry: %s is corrupt (%v) — starting with empty offsets", path, err)
		r.entries = map[string]*registryEntry{}
		return r, nil
	}
	r.prune()
	return r, nil
}

// Lookup returns the stored offset and fingerprint for a file id. A zero
// fingerprint means none was recorded — see registryEntry.Fingerprint.
func (r *OffsetRegistry) Lookup(id string) (offset int64, fingerprint uint64, ok bool) {
	if r == nil || id == "" {
		return 0, 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, found := r.entries[id]
	if !found {
		return 0, 0, false
	}
	return e.Offset, e.Fingerprint, true
}

// Reset returns a file to "start from the beginning, identity unknown".
//
// This is the only way the offset moves backwards, and it exists for exactly
// one situation: a file truncated in place (logrotate's copytruncate). The
// fingerprint is cleared along with the offset because the file now holds
// different content under the same inode, and a stale checksum would make the
// next open misread this as a recycled inode.
func (r *OffsetRegistry) Reset(id, path string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entry(id)
	e.Path = path
	e.Offset = 0
	e.Fingerprint = 0
	e.Updated = time.Now().UTC()
	r.dirty = true
}

// Commit advances the recorded offset for a file, never moving it backwards.
//
// This is the normal path, and it is separate from Set because it is called
// from two goroutines that have no ordering relationship: the exporter's sender
// records what it delivered, and the daemon records what the aggregator
// absorbed. Both walk the same file forwards, but a stale update arriving after
// a newer one must not rewind the offset — that would re-send lines that were
// already accounted for on the next restart. Taking the max makes the operation
// commutative, so the order the two callers happen to run in stops mattering.
func (r *OffsetRegistry) Commit(id, path string, offset int64, fingerprint uint64) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	e, existed := r.entries[id]
	if !existed {
		// First time we have committed anything for this file. Worth persisting
		// even at offset zero: "known, nothing accounted for yet" and "never
		// seen" lead to different decisions at startup — the first resumes from
		// the beginning, the second skips to the end so a new install does not
		// replay whatever history happens to be on disk.
		e = &registryEntry{}
		r.entries[id] = e
		e.set(path, offset, fingerprint)
		r.dirty = true
		return
	}

	if offset <= e.Offset {
		// Already accounted for. Still refresh Updated so an actively-tailed
		// file is never pruned as stale just because its offset stopped moving.
		e.Updated = time.Now().UTC()
		return
	}
	e.set(path, offset, fingerprint)
	r.dirty = true
}

// entry returns the entry for id, creating it if needed. Caller must hold r.mu.
func (r *OffsetRegistry) entry(id string) *registryEntry {
	e, ok := r.entries[id]
	if !ok {
		e = &registryEntry{}
		r.entries[id] = e
	}
	return e
}

func (e *registryEntry) set(path string, offset int64, fingerprint uint64) {
	e.Path = path
	e.Offset = offset
	e.Updated = time.Now().UTC()
	// Never overwrite a known fingerprint with zero: a file can be too short to
	// fingerprint at open and grow past the threshold later, and losing the
	// value we already had would give up the inode-reuse protection.
	if fingerprint != 0 {
		e.Fingerprint = fingerprint
	}
}

// Flush writes the registry to disk if anything changed since the last write.
// The write goes to a temp file in the same directory and is then renamed, so
// a crash mid-write leaves the previous registry intact rather than a
// half-written one that would fail to parse on the next boot.
func (r *OffsetRegistry) Flush() error {
	if r == nil || r.path == "" {
		// Persistence disabled because the directory was not writable at
		// startup; in-memory offsets still work for this process.
		return nil
	}
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	r.prune()
	raw, err := json.Marshal(r.entries)
	r.dirty = false
	r.mu.Unlock()
	if err != nil {
		return fmt.Errorf("marshaling offset registry: %w", err)
	}

	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("writing offset registry: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("replacing offset registry: %w", err)
	}
	return nil
}

// prune drops entries for files not seen recently. Caller must hold r.mu.
func (r *OffsetRegistry) prune() {
	cutoff := time.Now().UTC().Add(-registryMaxAge)
	for id, e := range r.entries {
		if e.Updated.Before(cutoff) {
			delete(r.entries, id)
			r.dirty = true
		}
	}
}

// Internal labels carrying tail provenance on an Envelope. The underscore
// prefix is the established convention for plumbing that rides along with a
// signal but is not part of it: the exporter strips these before building OTLP
// attributes and the dashboard strips them before display, so they never reach
// a backend or a screen.
//
// They exist because the offset can only be committed by whoever discovers that
// a line has been dealt with, and that happens well downstream of the tailer —
// in the exporter's sender goroutine, or in the daemon when the aggregator
// absorbs a request. Carrying the position on the envelope is what lets those
// places commit it without knowing anything about files.
const (
	LabelTailID  = "_tail_id"
	LabelTailEnd = "_tail_end"
	LabelTailFP  = "_tail_fp"
)

// tailProvenanceLabels renders a line's position as envelope labels.
func tailProvenanceLabels(l tailLine) map[string]string {
	if l.FileID == "" {
		return nil
	}
	m := map[string]string{
		LabelTailID:  l.FileID,
		LabelTailEnd: strconv.FormatInt(l.EndOffset, 10),
	}
	if l.Fingerprint != 0 {
		m[LabelTailFP] = strconv.FormatUint(l.Fingerprint, 10)
	}
	return m
}

// withTailProvenance merges the position labels into a label set a collector
// has already built.
func withTailProvenance(labels map[string]string, l tailLine) map[string]string {
	p := tailProvenanceLabels(l)
	if p == nil {
		return labels
	}
	if labels == nil {
		return p
	}
	for k, v := range p {
		labels[k] = v
	}
	return labels
}

// CommitTailOffset records that everything up to this envelope's line has been
// accounted for. Envelopes that did not come from a file are ignored.
//
// "Accounted for" deliberately means delivered OR deliberately discarded, not
// delivered alone. The exporter sheds its oldest envelopes under sustained
// backpressure by design; if a shed envelope blocked the offset forever, the
// agent would re-read from that point on every restart and never make progress.
// So a shed line is committed and counted as lost, which is the existing,
// visible behaviour — while a line still in the queue when the process dies is
// NOT committed, and is re-read. That second case is the one this fixes.
func CommitTailOffset(reg *OffsetRegistry, e Envelope) {
	if reg == nil || len(e.Labels) == 0 {
		return
	}
	id := e.Labels[LabelTailID]
	if id == "" {
		return
	}
	offset, err := strconv.ParseInt(e.Labels[LabelTailEnd], 10, 64)
	if err != nil {
		return
	}
	fp, _ := strconv.ParseUint(e.Labels[LabelTailFP], 10, 64)
	reg.Commit(id, e.Source, offset, fp)
}

// FileID returns a stable identifier for a file: its device and inode numbers.
// Returns "" on platforms where that isn't available, in which case callers
// degrade to not persisting offsets for that file rather than misattributing
// one.
func FileID(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
}
