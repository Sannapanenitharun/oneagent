package collector

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// Get returns the stored offset for a file id, and whether one existed.
func (r *OffsetRegistry) Get(id string) (int64, bool) {
	if r == nil || id == "" {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return 0, false
	}
	return e.Offset, true
}

// Set records the offset reached in the file identified by id.
func (r *OffsetRegistry) Set(id, path string, offset int64) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		e = &registryEntry{}
		r.entries[id] = e
	}
	e.Path = path
	e.Offset = offset
	e.Updated = time.Now().UTC()
	r.dirty = true
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
