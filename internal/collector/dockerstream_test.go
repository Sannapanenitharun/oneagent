package collector

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// frame builds one multiplexed stream frame the way dockerd writes it.
func frame(stream byte, payload string) []byte {
	h := make([]byte, dockerStreamHeaderLen)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

// decodeStream runs a decoder over r and returns everything it emitted.
func decodeStream(t *testing.T, dec *dockerStreamDecoder, r io.Reader) ([]dockerStreamLine, error) {
	t.Helper()
	var got []dockerStreamLine
	err := dec.Decode(r, func(l dockerStreamLine) { got = append(got, l) })
	return got, err
}

const ts1 = "2026-08-31T09:19:00.123456789Z"

// The basic contract: stdout and stderr arrive on one connection and must come
// out attributed to the stream they were written on, with the daemon's
// timestamp lifted off the front of the message.
func TestDockerStreamDecoder_SplitsStreamsAndParsesTimestamps(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, ts1+" hello from stdout\n"))
	in.Write(frame(2, "2026-08-31T09:19:01Z oh no\n"))

	got, err := decodeStream(t, newDockerStreamDecoder(false, 0), &in)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Decode ended with %v, want io.EOF", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(got), got)
	}
	if got[0].Stream != "stdout" || got[0].Text != "hello from stdout" {
		t.Errorf("stdout line = %+v", got[0])
	}
	if got[1].Stream != "stderr" || got[1].Text != "oh no" {
		t.Errorf("stderr line = %+v", got[1])
	}
	want, _ := time.Parse(time.RFC3339Nano, ts1)
	if !got[0].At.Equal(want) {
		t.Errorf("timestamp = %v, want %v", got[0].At, want)
	}
}

// A frame is a transport unit, not a record. The daemon flushes when it feels
// like it, so one line routinely arrives in several frames — and treating a
// frame boundary as a line boundary would chop log lines at arbitrary points.
func TestDockerStreamDecoder_ReassemblesLineAcrossFrames(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, ts1+" first hal"))
	in.Write(frame(1, "f and second half\n"))

	got, _ := decodeStream(t, newDockerStreamDecoder(false, 0), &in)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(got), got)
	}
	if got[0].Text != "first half and second half" {
		t.Errorf("Text = %q", got[0].Text)
	}
}

// And the reverse: several records in one frame must not merge.
func TestDockerStreamDecoder_SplitsSeveralLinesInOneFrame(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, ts1+" one\n"+ts1+" two\n"+ts1+" three\n"))

	got, _ := decodeStream(t, newDockerStreamDecoder(false, 0), &in)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(got), got)
	}
	for i, want := range []string{"one", "two", "three"} {
		if got[i].Text != want {
			t.Errorf("line %d = %q, want %q", i, got[i].Text, want)
		}
	}
}

// Interleaving is the reason the buffer is per stream. A partial stdout record
// with a complete stderr record delivered in between must not absorb it.
func TestDockerStreamDecoder_InterleavedStreamsDoNotContaminate(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, ts1+" out-start"))
	in.Write(frame(2, ts1+" err-whole\n"))
	in.Write(frame(1, "-out-end\n"))

	got, _ := decodeStream(t, newDockerStreamDecoder(false, 0), &in)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(got), got)
	}
	if got[0].Stream != "stderr" || got[0].Text != "err-whole" {
		t.Errorf("first = %+v, want the complete stderr record", got[0])
	}
	if got[1].Stream != "stdout" || got[1].Text != "out-start-out-end" {
		t.Errorf("second = %+v, want the reassembled stdout record", got[1])
	}
}

// A container started with a TTY has no framing at all: the pty merged the two
// streams, so the daemon sends raw bytes. Decoding those as frames would read
// the first eight bytes of a log line as a header.
func TestDockerStreamDecoder_TTYStreamIsUnframed(t *testing.T) {
	in := strings.NewReader(ts1 + " raw line one\n" + ts1 + " raw line two\n")

	got, _ := decodeStream(t, newDockerStreamDecoder(true, 0), in)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(got), got)
	}
	if got[0].Text != "raw line one" || got[1].Text != "raw line two" {
		t.Errorf("lines = %q, %q", got[0].Text, got[1].Text)
	}
	if got[0].Stream != "stdout" {
		t.Errorf("stream = %q, want stdout: a tty has only one", got[0].Stream)
	}
}

// A stream that never emits a newline must not buffer without limit. The
// behaviour has to match the file tailer's exactly — truncate once, mark, then
// drop the rest of the record — because the two paths produce the same signal
// and disagreeing about it would make container logs depend on which reader the
// host happened to select.
func TestDockerStreamDecoder_TruncatesThenDiscardsRemainder(t *testing.T) {
	const max = 64
	long := strings.Repeat("x", 500)

	var in bytes.Buffer
	in.Write(frame(1, ts1+" "+long+"\n"))
	in.Write(frame(1, ts1+" after\n"))

	got, _ := decodeStream(t, newDockerStreamDecoder(false, max), &in)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (the truncated record and the next one): %+v", len(got), got)
	}
	if !strings.HasSuffix(got[0].Text, truncatedSuffix) {
		t.Errorf("first line is not marked truncated: %q", got[0].Text)
	}
	if got[1].Text != "after" {
		t.Errorf("second line = %q, want %q — the discard must stop at the newline, "+
			"not swallow the record after it", got[1].Text, "after")
	}
}

// The buffer must actually be released, not merely marked. A stream that only
// ever produces over-long records would otherwise hold the last one forever.
func TestDockerStreamDecoder_DoesNotRetainBufferAfterTruncation(t *testing.T) {
	dec := newDockerStreamDecoder(false, 32)
	var in bytes.Buffer
	in.Write(frame(1, ts1+" "+strings.Repeat("y", 400)))

	if _, err := decodeStream(t, dec, &in); !errors.Is(err, io.EOF) {
		t.Fatalf("Decode ended with %v", err)
	}
	if got := len(dec.partial["stdout"]); got != 0 {
		t.Errorf("retained %d buffered bytes after truncating; want 0", got)
	}
}

// Losing framing means every subsequent byte would be misread, so the decoder
// stops and lets the caller reconnect on a fresh frame boundary rather than
// emitting garbage as log lines.
func TestDockerStreamDecoder_RejectsInvalidFraming(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(7, "not a real stream byte\n"))

	_, err := decodeStream(t, newDockerStreamDecoder(false, 0), &in)
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("Decode returned %v, want a framing error", err)
	}
	if !strings.Contains(err.Error(), "framing lost") {
		t.Errorf("error = %v, want it to say framing was lost", err)
	}
}

// A record without a parseable timestamp prefix still has to be delivered: the
// alternative is losing a real log line to a formatting detail. It carries a
// zero time, which is the signal to the emitter not to advance the cursor.
func TestParseDockerStreamLine_KeepsTextWhenTimestampIsAbsent(t *testing.T) {
	l := parseDockerStreamLine("stdout", "no-timestamp-here")
	if l.Text != "no-timestamp-here" {
		t.Errorf("Text = %q, want the line intact", l.Text)
	}
	if !l.At.IsZero() {
		t.Errorf("At = %v, want zero so the resume cursor is not advanced", l.At)
	}

	l = parseDockerStreamLine("stdout", "not-a-time but has a space")
	if l.Text != "not-a-time but has a space" {
		t.Errorf("Text = %q, want the whole line when the first field is not a timestamp", l.Text)
	}
	if !l.At.IsZero() {
		t.Errorf("At = %v, want zero", l.At)
	}
}

// An unterminated tail at EOF is held, not emitted. The container may simply
// not have written its newline yet, and a reconnect resumes from the last
// delivered timestamp — so holding costs nothing while emitting would split one
// record into two.
func TestDockerStreamDecoder_HoldsUnterminatedTailAtEOF(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, ts1+" complete\n"))
	in.Write(frame(1, ts1+" still being written"))

	got, err := decodeStream(t, newDockerStreamDecoder(false, 0), &in)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Decode ended with %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines, want only the complete one: %+v", len(got), got)
	}
	if got[0].Text != "complete" {
		t.Errorf("Text = %q", got[0].Text)
	}
}
