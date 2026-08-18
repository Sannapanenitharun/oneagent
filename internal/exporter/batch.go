package exporter

import "github.com/agent-i/agent/internal/collector"

// defaultMaxBatchBytes caps the encoded size of one outgoing request.
//
// Before this existed, both network exporters flushed on record COUNT alone.
// That is fine while records are small and silently wrong once they are not:
// a batch of 100 log records is a few hundred KiB of syslog lines, or 400 MiB
// of joined stack traces. Multi-line assembly made the second case reachable
// in normal operation rather than only under abuse - one record may now hold
// up to logs.multiline.max_lines lines.
//
// 4 MiB rather than a rounder number because it is exactly what this agent's
// own OTLP receiver accepts (traces.max_request_bytes). Without the cap the
// exporter could build a request the receiver in the same binary would refuse,
// which is a contract the agent should not be able to break with itself.
const defaultMaxBatchBytes = 4 << 20

// envelopeOverhead approximates the per-record encoding cost not covered by
// the fields envelopeBytes measures: OTLP resource/scope wrappers, timestamps,
// severity, and the framing around each attribute. Rounded up rather than
// derived, because the only estimation error that hurts here is one that
// under-counts.
const envelopeOverhead = 256

// maxPayloadDepth bounds how far envelopeBytes walks into a nested payload.
// Span payloads nest a few levels; anything deeper is charged a flat rate
// rather than traversed, so a pathological structure cannot make estimating
// its size more expensive than sending it.
const maxPayloadDepth = 4

// envelopeBytes estimates the encoded size of one envelope.
//
// An estimate, not a measurement. Encoding a batch once to find its size and
// again to send it would double the CPU cost of every export, and the cap does
// not need to be exact - it needs to keep a request under whatever limit the
// backend enforces. The estimate is therefore biased HIGH: flushing a little
// early costs one extra HTTP request, while under-counting costs the entire
// batch to a 413 that no retry can fix.
func envelopeBytes(e collector.Envelope) int {
	n := envelopeOverhead + len(e.Source) + len(e.Message) + len(e.AgentID)
	for k, v := range e.Labels {
		n += len(k) + len(v) + 8 // 8 covers the key/value framing per attribute
	}
	return n + payloadBytes(e.Payload, 0)
}

// payloadBytes walks a payload map, charging strings their length and
// everything else a flat constant.
func payloadBytes(p map[string]any, depth int) int {
	if p == nil || depth > maxPayloadDepth {
		return 0
	}
	n := 0
	for k, v := range p {
		n += len(k) + 8
		n += valueBytes(v, depth)
	}
	return n
}

func valueBytes(v any, depth int) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case []byte:
		return len(t)
	case map[string]any:
		return payloadBytes(t, depth+1)
	case []any:
		if depth > maxPayloadDepth {
			return len(t) * 16
		}
		n := 0
		for _, item := range t {
			n += valueBytes(item, depth+1)
		}
		return n
	case nil:
		return 4
	default:
		// Numbers and bools. 24 is generous for every numeric encoding OTLP
		// uses, which is the direction to err in.
		return 24
	}
}

// resolveMaxBatchBytes applies the default for an unset or invalid setting.
func resolveMaxBatchBytes(configured int) int {
	if configured <= 0 {
		return defaultMaxBatchBytes
	}
	return configured
}
