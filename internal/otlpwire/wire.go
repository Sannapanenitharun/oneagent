// Package otlpwire decodes the subset of the protobuf wire format that the
// OTLP trace receiver needs, plus the OTLP trace message types themselves.
//
// It exists to replace google.golang.org/protobuf and go.opentelemetry.io/proto
// — together ~61k lines vendored — with the roughly 1% of their behaviour this
// agent actually used: decoding one inbound ExportTraceServiceRequest. None of
// protobuf-go's generality (reflection, descriptors, Any, protojson, dynamic
// messages) was reachable from this codebase.
//
// The wire format is deliberately simple and is a published specification
// (protobuf.dev/programming-guides/encoding). Every message is a flat sequence
// of (tag, value) pairs: the tag is a varint holding a field number and a wire
// type, and the wire type says how to find the end of the value. That is enough
// to walk any message without knowing its schema, which is what makes skipping
// unknown fields — the thing that keeps this forward-compatible with newer OTLP
// producers — a few lines rather than a design problem.
package otlpwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Wire types, from the encoding specification.
const (
	wireVarint     = 0
	wireFixed64    = 1
	wireBytes      = 2
	wireStartGroup = 3
	wireEndGroup   = 4
	wireFixed32    = 5
)

var (
	errTruncated = errors.New("otlpwire: truncated message")
	errBadVarint = errors.New("otlpwire: malformed varint")
)

// maxNestingDepth bounds how deep a decoder will recurse.
//
// Length-delimited fields nest, so a hostile payload only a few hundred bytes
// long can describe thousands of levels of nesting and exhaust the goroutine
// stack — and the receiver's max_request_bytes limit is no help, because the
// attack is depth rather than size.
//
// As written this guard is unreachable, and that is worth being explicit about
// rather than leaving as a false sense of safety: the decoders here do not
// recurse generically, they descend only where the OTLP trace schema allows,
// and that schema bottoms out around five levels. The one construct that could
// make it unbounded is AnyValue's array_value/kvlist_value, which nest
// arbitrarily — and those are currently skipped rather than decoded.
//
// It is kept because decoding them is the obvious future change, and it is much
// easier to leave the bound in place now than to remember it later.
const maxNestingDepth = 64

// reader walks a protobuf-encoded buffer. The zero value is not useful; use
// newReader. It never panics on malformed input — every read is bounds-checked
// and reports an error instead, because this decodes bytes arriving from the
// network.
type reader struct {
	buf   []byte
	pos   int
	depth int
}

func newReader(b []byte, depth int) *reader { return &reader{buf: b, depth: depth} }

func (r *reader) done() bool { return r.pos >= len(r.buf) }

// varint reads a base-128 varint: seven bits of payload per byte, high bit set
// on every byte but the last.
func (r *reader) varint() (uint64, error) {
	var v uint64
	var shift uint
	for {
		if r.pos >= len(r.buf) {
			return 0, errTruncated
		}
		b := r.buf[r.pos]
		r.pos++
		// 10 bytes is the maximum for a 64-bit value (9*7=63 bits, plus one
		// more byte for the 64th). Beyond that the value cannot be
		// represented and the input is malformed rather than merely large.
		if shift >= 64 {
			return 0, errBadVarint
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
}

// tag reads a field tag, returning the field number and wire type.
func (r *reader) tag() (field int, wire int, err error) {
	t, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	field = int(t >> 3)
	wire = int(t & 0x7)
	if field <= 0 {
		return 0, 0, fmt.Errorf("otlpwire: invalid field number %d", field)
	}
	return field, wire, nil
}

// bytes reads a length-delimited value, returning a subslice of the input.
// The result aliases the caller's buffer; callers that retain it past the life
// of the request must copy (see Span.TraceId).
func (r *reader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	// Compared against the remaining input, not just len(buf): a declared
	// length of 2^40 must be rejected rather than used to index.
	if n > uint64(len(r.buf)-r.pos) {
		return nil, errTruncated
	}
	start := r.pos
	r.pos += int(n)
	return r.buf[start:r.pos], nil
}

func (r *reader) fixed64() (uint64, error) {
	if r.pos+8 > len(r.buf) {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *reader) fixed32() (uint32, error) {
	if r.pos+4 > len(r.buf) {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

// skip advances past a field whose value we do not care about.
//
// This is what makes the decoder forward-compatible: an OTLP producer newer
// than this agent will send fields this code has never heard of, and the
// correct response is to walk past them, not to reject the payload. Every
// message decoder below funnels its default case here.
func (r *reader) skip(wire int) error {
	switch wire {
	case wireVarint:
		_, err := r.varint()
		return err
	case wireFixed64:
		_, err := r.fixed64()
		return err
	case wireBytes:
		_, err := r.bytes()
		return err
	case wireFixed32:
		_, err := r.fixed32()
		return err
	case wireStartGroup, wireEndGroup:
		// Groups were removed from the language in proto3 and no OTLP
		// message uses them. Rejecting is safer than attempting to skip a
		// construct we would then never exercise or test.
		return errors.New("otlpwire: group wire types are not supported")
	default:
		return fmt.Errorf("otlpwire: unknown wire type %d", wire)
	}
}

// sub returns a reader over a nested message, enforcing the depth limit.
func (r *reader) sub(b []byte) (*reader, error) {
	if r.depth+1 > maxNestingDepth {
		return nil, fmt.Errorf("otlpwire: message nested deeper than %d levels", maxNestingDepth)
	}
	return newReader(b, r.depth+1), nil
}

func float64from(bits uint64) float64 { return math.Float64frombits(bits) }
