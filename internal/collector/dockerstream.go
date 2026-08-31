package collector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

// Docker's log endpoint returns one of two wire formats, and which one is not
// negotiable — it follows from how the container was started.
//
// A container without a TTY has separate stdout and stderr, so the daemon
// multiplexes them into a single connection with an 8-byte header per frame:
//
//	byte 0     stream: 1 = stdout, 2 = stderr (0 = stdin, never sent here)
//	bytes 1-3  zero padding
//	bytes 4-7  payload length, big endian
//
// A container WITH a TTY has only one stream — the pty merges them — so the
// daemon sends the bytes with no framing at all. There is no in-band marker
// distinguishing the two, which is why the caller asks the daemon (see
// dockerClient.TTY) rather than sniffing: a raw log line beginning with byte
// 0x01 is indistinguishable from a stdout frame header.
//
// Frames are not lines. One frame can carry several lines, and one line can
// span many frames, so the decoder reassembles per stream rather than treating
// a frame boundary as a record boundary.
const dockerStreamHeaderLen = 8

// dockerStreamLine is one complete log record lifted out of a stream.
type dockerStreamLine struct {
	// Stream is "stdout" or "stderr".
	Stream string
	// At is the daemon's timestamp for the record. It is also the resume
	// cursor: the collector records the newest one it has delivered and asks
	// for logs after it on the next connection.
	At time.Time
	// Text is the message with the timestamp prefix and trailing newline
	// removed.
	Text string
}

// dockerStreamDecoder turns a log stream into complete lines.
//
// It holds a partial-line buffer per stream, which is why it is a type rather
// than a function: a line split across frames — or across two reads of the same
// frame — must not surface as two records.
type dockerStreamDecoder struct {
	// tty selects the unframed format.
	tty bool
	// maxLine caps a single record. Zero means unbounded, which is only
	// sensible in tests.
	maxLine int

	partial map[string][]byte
	// discarding marks a stream whose current record was already emitted
	// truncated, so the rest of it is dropped instead of arriving as a second
	// record that begins mid-sentence. This mirrors the file tailer's
	// skipToNewline exactly; the two paths must not disagree about what an
	// over-long line does.
	discarding map[string]bool
}

func newDockerStreamDecoder(tty bool, maxLine int) *dockerStreamDecoder {
	return &dockerStreamDecoder{
		tty:        tty,
		maxLine:    maxLine,
		partial:    map[string][]byte{},
		discarding: map[string]bool{},
	}
}

// Decode reads r to completion, calling emit for every complete line.
//
// It returns the error that ended the stream, including io.EOF when the
// container exited cleanly. A partial line left buffered at EOF is deliberately
// NOT emitted: the container may simply not have written its newline yet, and a
// reconnect resumes from the last delivered timestamp, so holding it costs
// nothing while emitting it would split one record in two.
func (d *dockerStreamDecoder) Decode(r io.Reader, emit func(dockerStreamLine)) error {
	// One chunk buffer for the life of the stream. Frames are read through it
	// in pieces rather than allocated per frame, so a daemon reporting an
	// absurd frame length cannot make the agent allocate it.
	chunk := make([]byte, 32*1024)
	header := make([]byte, dockerStreamHeaderLen)

	for {
		if d.tty {
			n, err := r.Read(chunk)
			if n > 0 {
				d.feed("stdout", chunk[:n], emit)
			}
			if err != nil {
				return err
			}
			continue
		}

		if _, err := io.ReadFull(r, header); err != nil {
			return err
		}
		stream, err := dockerStreamName(header[0])
		if err != nil {
			// Framing is lost. Continuing would emit garbage as log lines, so
			// the stream is abandoned and the caller reconnects, which
			// resynchronises on a fresh frame boundary.
			return err
		}
		remaining := int64(binary.BigEndian.Uint32(header[4:8]))
		for remaining > 0 {
			n := int64(len(chunk))
			if remaining < n {
				n = remaining
			}
			if _, err := io.ReadFull(r, chunk[:n]); err != nil {
				return err
			}
			d.feed(stream, chunk[:n], emit)
			remaining -= n
		}
	}
}

// dockerStreamName maps a frame's stream byte to a name.
func dockerStreamName(b byte) (string, error) {
	switch b {
	case 1:
		return "stdout", nil
	case 2:
		return "stderr", nil
	case 0:
		// stdin is never replayed by the log endpoint. Seeing it means the
		// header was read at the wrong offset.
		return "", fmt.Errorf("docker log stream: unexpected stdin frame — framing lost")
	default:
		return "", fmt.Errorf("docker log stream: invalid stream byte %d — framing lost", b)
	}
}

// feed appends bytes to one stream's buffer and emits whatever lines complete.
func (d *dockerStreamDecoder) feed(stream string, p []byte, emit func(dockerStreamLine)) {
	buf := append(d.partial[stream], p...)

	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := buf[:i]
		buf = buf[i+1:]

		if d.discarding[stream] {
			// This newline ends the record that was already emitted truncated.
			delete(d.discarding, stream)
			continue
		}
		d.emitLine(stream, string(line), false, emit)
	}

	// A stream that never emits a newline must not buffer without limit. Cut
	// the record, mark it, and drop the rest of it when it finally ends.
	if !d.discarding[stream] && d.maxLine > 0 && len(buf) > d.maxLine {
		d.emitLine(stream, string(buf[:d.maxLine]), true, emit)
		d.discarding[stream] = true
		buf = buf[:0]
	}

	if len(buf) == 0 {
		// Dropping the entry rather than keeping an empty slice matters on a
		// host that churns containers: the decoder is per stream, but the maps
		// would otherwise retain a key for every stream that ever produced a
		// byte.
		delete(d.partial, stream)
		return
	}
	d.partial[stream] = buf
}

// emitLine parses one raw record and applies the length cap to its message.
//
// The cap is applied after the timestamp is split off, not before, for two
// reasons: the limit an operator sets means the size of a log line, not the
// size of a line plus thirty bytes of RFC3339; and cutting the raw record first
// would destroy the timestamp outright at any cap smaller than its prefix,
// which would also stop the resume cursor advancing.
//
// truncated is passed in because the caller knows something the length cannot
// show: a record cut at the buffer limit has already lost its tail, and must be
// marked even though what survived is now under the cap.
func (d *dockerStreamDecoder) emitLine(stream, raw string, truncated bool, emit func(dockerStreamLine)) {
	l := parseDockerStreamLine(stream, raw)
	if d.maxLine > 0 && len(l.Text) > d.maxLine {
		l.Text = l.Text[:d.maxLine]
		truncated = true
	}
	if truncated {
		l.Text += truncatedSuffix
	}
	emit(l)
}

// parseDockerStreamLine splits the RFC3339Nano timestamp the daemon prefixes
// when timestamps=1 from the message it belongs to.
//
// A line without a parseable prefix keeps its text intact and gets a zero time.
// That happens for the first partial record when a stream is resumed mid-line,
// and the alternative — dropping it — would lose a real log line to a
// formatting detail.
func parseDockerStreamLine(stream, raw string) dockerStreamLine {
	raw = strings.TrimSuffix(raw, "\r")
	ts, msg, ok := strings.Cut(raw, " ")
	if !ok {
		return dockerStreamLine{Stream: stream, Text: raw}
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return dockerStreamLine{Stream: stream, Text: raw}
	}
	return dockerStreamLine{Stream: stream, At: at.UTC(), Text: msg}
}
