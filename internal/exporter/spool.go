package exporter

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

// spool is a bounded, append-only, disk-backed buffer behind the exporter's
// in-memory queue.
//
// It exists because the queue alone loses data in two situations that are not
// hypothetical. An outage longer than the queue is deep drops the oldest
// envelopes permanently — at the default 4096 slots that is seconds of
// telemetry on a busy host. And any restart, graceful or not, discards
// whatever was still queued: the shutdown drain used to log a count and give
// up.
//
// Tailed files and the journal never had this problem, because the source is
// itself a durable log and the agent only has to remember a position — see
// OffsetRegistry and collector.CursorStore, both committed on retire rather
// than on read. Nothing pushed into the OTLP receiver has that property. The
// application already handed the span over and will not offer it twice, so if
// the agent does not keep it, no one has it. That is the hole this fills.
//
// # Overflow, not write-ahead
//
// Envelopes reach the spool only once the in-memory queue is full, or at
// shutdown. The healthy path does no disk I/O at all, which is the whole
// reason to prefer this over a true write-ahead log: an agent that fsyncs
// every metric sample it collects has become a load source on the host it is
// supposed to be observing.
//
// The cost of that choice is a residual window. A hard kill while the backend
// is healthy still loses what was in the queue — but a healthy sender keeps
// the queue near empty, so that window is small, and it covers exactly the
// envelopes that were about to be delivered anyway. Data that sat around long
// enough to be worth protecting is data that filled the queue and reached
// disk.
//
// # Layout
//
// The directory holds numbered segments and one position file. Records are
//
//	uint32 payload length (big endian)
//	uint32 CRC32-C of the payload
//	payload — the JSON envelope
//
// Reads come from the oldest segment, writes go to the newest, and a segment
// is deleted once the reader passes its end. Truncation is therefore free:
// nothing is ever rewritten or compacted, which is what keeps a spool under
// sustained overflow from spending more I/O on housekeeping than on data.
//
// A fresh segment is opened at startup rather than appending to the last one.
// A crash can only ever tear the tail of the segment being written, so making
// that segment read-only on the next run means a torn record is always the
// last thing in its file — the reader stops there, drops the segment and moves
// on. No torn record can appear in the middle of a live file, where it would
// block everything behind it forever.
//
// # Guarantees
//
// At-least-once, deliberately. The read position is committed periodically,
// not per record, so a crash replays the last few delivered envelopes. The
// alternative — an fsync per envelope — costs more than duplicate telemetry
// does, and every backend worth pushing to already tolerates duplicates.
type spool struct {
	opts spoolOptions

	mu sync.Mutex

	seq    uint64
	w      *os.File
	wName  string
	wBytes int64

	r     *os.File
	rName string
	rOff  int64

	// peeked records that Peek handed out an envelope the caller has not yet
	// acknowledged. The read position does not move until Ack, so a delivery
	// that fails re-offers the same envelope instead of losing it.
	peeked  bool
	peekEnd int64

	// unread and total are maintained in memory rather than by stat'ing the
	// directory, because Export consults empty() on every single envelope.
	unread int64
	total  int64

	sinceCommit int
	lastSync    time.Time
	dirty       bool

	closed bool
}

type spoolOptions struct {
	Dir          string
	MaxBytes     int64
	SegmentBytes int64
	SyncInterval time.Duration
	// Retire is called for every envelope discarded to stay under MaxBytes,
	// for the same reason the in-memory queue reports its shed envelopes: if a
	// deliberate drop did not settle, a tailed file's offset would never
	// advance past it and the agent would re-read the same bytes after every
	// restart without ever making progress.
	//
	// It runs on the caller's goroutine with the spool lock held, so it must
	// not call back into the exporter — Export consults the spool on every
	// envelope and would deadlock against it.
	Retire func(collector.Envelope)
}

const (
	spoolHeaderBytes = 8
	// spoolMaxRecordBytes bounds what a length header is allowed to claim. A
	// torn or corrupt header would otherwise ask the reader to allocate an
	// arbitrary amount of memory before the CRC ever got the chance to reject
	// it.
	spoolMaxRecordBytes = 16 << 20

	defaultSpoolMaxBytes     = 128 << 20
	defaultSpoolSegmentBytes = 8 << 20
	defaultSpoolSyncInterval = time.Second
	minSpoolSegmentBytes     = 64 << 10

	// spoolCommitEvery bounds how many delivered envelopes a crash can replay.
	// Committing per record would put an fsync-sized cost on the drain path
	// for the sake of duplicates the backend already tolerates.
	spoolCommitEvery = 64

	spoolSegmentSuffix = ".spl"
	spoolPositionName  = "position"
)

// Castagnoli rather than IEEE: it is hardware-accelerated on every CPU this
// agent ships to, so integrity checking costs nothing measurable.
var spoolCRC = crc32.MakeTable(crc32.Castagnoli)

var errSpoolClosed = errors.New("spool is closed")

func openSpool(opts spoolOptions) (*spool, error) {
	if opts.Dir == "" {
		return nil, errors.New("spool requires a directory")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultSpoolMaxBytes
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = defaultSpoolSegmentBytes
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = defaultSpoolSyncInterval
	}
	// A segment is only reclaimable once fully read, so the cap has to hold at
	// least two of them: one being written and one still being drained. A
	// single-segment spool could never shed anything and would grow past its
	// own limit.
	if opts.SegmentBytes > opts.MaxBytes/2 {
		opts.SegmentBytes = opts.MaxBytes / 2
	}
	if opts.SegmentBytes < minSpoolSegmentBytes {
		opts.SegmentBytes = minSpoolSegmentBytes
	}
	// 0700: envelopes carry log lines and span attributes, which routinely
	// contain things the rest of the host has no business reading.
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating spool directory %s: %w", opts.Dir, err)
	}

	s := &spool{opts: opts, lastSync: time.Now()}
	if err := s.recover(); err != nil {
		return nil, err
	}
	if err := s.rotateLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// recover reconstructs the read position from the previous run and discards
// the segments that run already finished with.
func (s *spool) recover() error {
	segs, err := s.segments()
	if err != nil {
		return err
	}
	name, off := s.readPosition()

	// The position is only evidence about what was consumed if the segment it
	// names is still on disk. Pointing at a file that is gone — a corrupt
	// position, or one written by a different spool — says nothing about which
	// of the remaining segments were drained, and deleting them on that basis
	// would throw real data away to avoid re-reading it. Start over from the
	// oldest instead; the cost is duplicates, which is the cost this design
	// already accepts everywhere else.
	present := false
	for _, sg := range segs {
		if sg == name {
			present = true
			break
		}
	}
	if !present {
		name, off = "", 0
	}

	keep := make([]string, 0, len(segs))
	for _, sg := range segs {
		// Segment names sort lexicographically in write order, so anything
		// before the committed one was fully drained before the last exit.
		if name != "" && sg < name {
			_ = os.Remove(filepath.Join(s.opts.Dir, sg))
			continue
		}
		keep = append(keep, sg)
	}
	if len(keep) == 0 || keep[0] != name {
		off = 0
	}

	// Every start opens a fresh segment, so a run that never overflowed leaves
	// an empty one behind. The reader would eventually step over them, but
	// clearing them here keeps a host that restarts often from accumulating a
	// directory full of nothing.
	kept := keep[:0]
	for _, sg := range keep {
		if fi, err := os.Stat(filepath.Join(s.opts.Dir, sg)); err == nil && fi.Size() == 0 {
			_ = os.Remove(filepath.Join(s.opts.Dir, sg))
			if n, ok := segmentSeq(sg); ok && n > s.seq {
				s.seq = n // still claimed the number; do not reuse it
			}
			continue
		}
		kept = append(kept, sg)
	}
	keep = kept

	for i, sg := range keep {
		fi, err := os.Stat(filepath.Join(s.opts.Dir, sg))
		if err != nil {
			continue
		}
		size := fi.Size()
		s.total += size
		if i == 0 {
			if off > size || off < 0 {
				off = 0 // truncated or corrupt position file
			}
			s.unread += size - off
		} else {
			s.unread += size
		}
		if n, ok := segmentSeq(sg); ok && n > s.seq {
			s.seq = n
		}
	}
	if len(keep) > 0 {
		s.rName, s.rOff = keep[0], off
	}
	return nil
}

func (s *spool) segments() ([]string, error) {
	entries, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("reading spool directory %s: %w", s.opts.Dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), spoolSegmentSuffix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// segmentName zero-pads so lexicographic order is numeric order, which is what
// lets the reader pick the oldest segment with a plain string sort.
func segmentName(seq uint64) string {
	return fmt.Sprintf("%020d%s", seq, spoolSegmentSuffix)
}

func segmentSeq(name string) (uint64, bool) {
	base := strings.TrimSuffix(name, spoolSegmentSuffix)
	var n uint64
	if _, err := fmt.Sscanf(base, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// Append writes one envelope. It is called from the collector-facing side of
// the exporter, so it must not depend on the backend in any way: the only
// blocking it does is a local buffered write, plus an fsync at most once per
// SyncInterval.
func (s *spool) Append(e collector.Envelope) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding envelope for spool: %w", err)
	}
	if len(b) > spoolMaxRecordBytes {
		return fmt.Errorf("envelope of %d bytes exceeds the spool record limit", len(b))
	}
	rec := make([]byte, spoolHeaderBytes+len(b))
	binary.BigEndian.PutUint32(rec[0:4], uint32(len(b)))
	binary.BigEndian.PutUint32(rec[4:8], crc32.Checksum(b, spoolCRC))
	copy(rec[spoolHeaderBytes:], b)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSpoolClosed
	}
	// A record is never split across segments, so an oversized one gets a
	// segment to itself rather than being rejected.
	if s.wBytes > 0 && s.wBytes+int64(len(rec)) > s.opts.SegmentBytes {
		if err := s.rotateLocked(); err != nil {
			return err
		}
	}

	before := s.wBytes
	n, err := s.w.Write(rec)
	if err != nil {
		// Roll the partial record back. Left in place it would be a torn
		// record in the MIDDLE of a segment we are still appending to, and the
		// reader has no way past that — it would stall on it permanently while
		// the writer kept piling data in behind it. Truncating is the
		// difference between losing one envelope and losing the spool.
		if n > 0 {
			_ = s.w.Truncate(before)
			_, _ = s.w.Seek(before, 0)
		}
		return fmt.Errorf("appending to spool: %w", err)
	}
	s.wBytes += int64(n)
	s.total += int64(n)
	s.unread += int64(n)
	s.dirty = true

	s.maybeSyncLocked()
	s.enforceCapLocked()
	return nil
}

// Peek returns the oldest unread envelope without consuming it. The position
// only moves on Ack, so a failed delivery re-offers the same envelope.
func (s *spool) Peek() (collector.Envelope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return collector.Envelope{}, false
	}
	for {
		if s.r == nil && !s.openReaderLocked() {
			return collector.Envelope{}, false
		}
		env, end, status := s.readRecordLocked(s.rOff)
		switch status {
		case recordOK:
			s.peeked, s.peekEnd = true, end
			return env, true
		case recordSkip:
			// Intact on disk but not decodable as an envelope. Skipping past it
			// is the only option that does not stall every envelope behind it
			// on one bad record.
			s.unread -= end - s.rOff
			s.rOff = end
			continue
		default: // recordEnd
			if s.rName == s.wName {
				// Caught up with the writer. More may arrive; hold the position.
				return collector.Envelope{}, false
			}
			// A sealed segment ends here. Anything past this point is a torn
			// tail from an earlier crash and is unrecoverable by definition.
			s.dropReadSegmentLocked()
		}
	}
}

// Ack marks the peeked envelope as settled and advances past it.
func (s *spool) Ack() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.peeked {
		return
	}
	s.unread -= s.peekEnd - s.rOff
	s.rOff = s.peekEnd
	s.peeked = false
	s.sinceCommit++
	if s.sinceCommit >= spoolCommitEvery {
		s.commitLocked()
	}
}

// empty reports whether there is nothing left on disk to send.
func (s *spool) empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unread <= 0
}

// bytes is the undelivered backlog, for reporting.
func (s *spool) bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unread < 0 {
		return 0
	}
	return s.unread
}

// Commit persists the read position so a restart does not replay what has
// already been delivered.
func (s *spool) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitLocked()
}

// Sync forces the write segment to durable storage.
func (s *spool) Sync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
}

func (s *spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.syncLocked()
	s.commitLocked()
	var err error
	if s.w != nil {
		err = s.w.Close()
		s.w = nil
	}
	if s.r != nil {
		_ = s.r.Close()
		s.r = nil
	}
	return err
}

// --- internals, all called with mu held ---

type recordStatus int

const (
	recordOK recordStatus = iota
	recordSkip
	recordEnd
)

func (s *spool) readRecordLocked(off int64) (collector.Envelope, int64, recordStatus) {
	var hdr [spoolHeaderBytes]byte
	if _, err := s.r.ReadAt(hdr[:], off); err != nil {
		return collector.Envelope{}, 0, recordEnd
	}
	n := binary.BigEndian.Uint32(hdr[0:4])
	sum := binary.BigEndian.Uint32(hdr[4:8])
	if n == 0 || n > spoolMaxRecordBytes {
		return collector.Envelope{}, 0, recordEnd
	}
	buf := make([]byte, n)
	if _, err := s.r.ReadAt(buf, off+spoolHeaderBytes); err != nil {
		return collector.Envelope{}, 0, recordEnd // truncated tail
	}
	end := off + spoolHeaderBytes + int64(n)
	if crc32.Checksum(buf, spoolCRC) != sum {
		// If the payload is corrupt the length that framed it cannot be
		// trusted either, so this is the end of usable data in the segment
		// rather than a record to skip over.
		return collector.Envelope{}, 0, recordEnd
	}
	var e collector.Envelope
	if err := json.Unmarshal(buf, &e); err != nil {
		return collector.Envelope{}, end, recordSkip
	}
	return e, end, recordOK
}

func (s *spool) openReaderLocked() bool {
	segs, err := s.segments()
	if err != nil || len(segs) == 0 {
		return false
	}
	if s.rName == "" {
		s.rName, s.rOff = segs[0], 0
	}
	f, err := os.Open(filepath.Join(s.opts.Dir, s.rName))
	if err != nil {
		// Vanished under us — treat it as consumed and pick up the next one on
		// the following call.
		s.rName, s.rOff = "", 0
		return false
	}
	s.r = f
	return true
}

func (s *spool) dropReadSegmentLocked() {
	if s.rName == "" {
		return
	}
	path := filepath.Join(s.opts.Dir, s.rName)
	if fi, err := os.Stat(path); err == nil {
		s.total -= fi.Size()
		s.unread -= fi.Size() - s.rOff
	}
	if s.r != nil {
		_ = s.r.Close()
		s.r = nil
	}
	_ = os.Remove(path)
	s.rName, s.rOff, s.peeked = "", 0, false
	if s.unread < 0 {
		s.unread = 0
	}
	if s.total < 0 {
		s.total = 0
	}
	s.commitLocked()
}

// enforceCapLocked discards whole segments, oldest first, until the spool fits
// its limit. Oldest-first matches the in-memory queue's shed policy for the
// same reason: under sustained overload the freshest telemetry is the useful
// telemetry. And a spool that filled the disk instead would take the host down
// with it, which is a far worse outcome than losing the data it was
// protecting.
func (s *spool) enforceCapLocked() {
	for s.total > s.opts.MaxBytes {
		segs, err := s.segments()
		if err != nil || len(segs) == 0 {
			return
		}
		oldest := segs[0]
		if oldest == s.wName {
			return // nothing left to give up but the segment being written
		}
		s.shedSegmentLocked(oldest)
	}
}

func (s *spool) shedSegmentLocked(name string) {
	path := filepath.Join(s.opts.Dir, name)
	from := int64(0)
	if name == s.rName {
		from = s.rOff
	}
	if s.opts.Retire != nil {
		s.retireFromLocked(path, from)
	}
	if name == s.rName {
		s.dropReadSegmentLocked()
		return
	}
	if fi, err := os.Stat(path); err == nil {
		s.total -= fi.Size()
		s.unread -= fi.Size()
	}
	_ = os.Remove(path)
	if s.unread < 0 {
		s.unread = 0
	}
	if s.total < 0 {
		s.total = 0
	}
}

// retireFromLocked reads back the records about to be discarded so their
// sources can move on.
//
// This holds the lock across a whole segment, so Export stalls for as long as
// it takes to decode one — order tens of milliseconds for 8 MiB out of the
// page cache it was just written to. That is accepted rather than engineered
// around because of when it happens: only with the spool already at its cap,
// meaning the backend has been unreachable long enough to accumulate the full
// MaxBytes. A brief stall on the collector goroutine at that point is a much
// smaller problem than the one the agent is already having.
func (s *spool) retireFromLocked(path string, from int64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	prev := s.r
	s.r = f
	defer func() { s.r = prev }()

	for off := from; ; {
		env, end, status := s.readRecordLocked(off)
		if status == recordEnd {
			return
		}
		if status == recordOK {
			s.opts.Retire(env)
		}
		off = end
	}
}

func (s *spool) rotateLocked() error {
	if s.w != nil {
		s.syncLocked()
		_ = s.w.Close()
		s.w = nil
	}
	for attempt := 0; attempt < 8; attempt++ {
		s.seq++
		name := segmentName(s.seq)
		f, err := os.OpenFile(filepath.Join(s.opts.Dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue // a stale segment already claimed this number
			}
			return fmt.Errorf("creating spool segment %s: %w", name, err)
		}
		s.w, s.wName, s.wBytes = f, name, 0
		return nil
	}
	return fmt.Errorf("creating spool segment in %s: no free sequence number", s.opts.Dir)
}

func (s *spool) maybeSyncLocked() {
	if !s.dirty {
		return
	}
	if now := time.Now(); now.Sub(s.lastSync) >= s.opts.SyncInterval {
		s.syncLocked()
		s.lastSync = now
	}
}

func (s *spool) syncLocked() {
	if s.w == nil || !s.dirty {
		return
	}
	_ = s.w.Sync()
	s.dirty = false
}

// commitLocked writes the read position through a temporary file and a rename,
// so a crash mid-write leaves the previous position rather than a half-written
// one. A stale position costs duplicates; a corrupt one costs the whole spool.
func (s *spool) commitLocked() {
	s.sinceCommit = 0
	if s.rName == "" && s.rOff == 0 {
		_ = os.Remove(filepath.Join(s.opts.Dir, spoolPositionName))
		return
	}
	final := filepath.Join(s.opts.Dir, spoolPositionName)
	tmp := final + ".tmp"
	body := fmt.Sprintf("%s\n%d\n", s.rName, s.rOff)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return
	}
	_ = f.Sync()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, final)
}

func (s *spool) readPosition() (string, int64) {
	raw, err := os.ReadFile(filepath.Join(s.opts.Dir, spoolPositionName))
	if err != nil {
		return "", 0
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		return "", 0
	}
	name := strings.TrimSpace(lines[0])
	if !strings.HasSuffix(name, spoolSegmentSuffix) {
		return "", 0
	}
	var off int64
	if _, err := fmt.Sscanf(strings.TrimSpace(lines[1]), "%d", &off); err != nil || off < 0 {
		return "", 0
	}
	return name, off
}
