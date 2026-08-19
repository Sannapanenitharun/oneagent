package otlpwire

import "strconv"

// The OTLP trace message types, carrying only the fields this agent reads.
//
// Field numbers below come from the OTLP protocol definitions
// (opentelemetry-proto: trace/v1/trace.proto, common/v1/common.proto,
// resource/v1/resource.proto). Field numbers are the wire contract and are
// frozen by the spec, so they are safe to hard-code; the names beside them are
// only for the reader. Fields the agent does not use (events, links, dropped
// counts, schema_url, trace_state) are intentionally absent — they are skipped
// on the wire rather than decoded into memory nobody reads.

// ValueKind discriminates AnyValue's oneof.
type ValueKind uint8

const (
	ValueEmpty ValueKind = iota
	ValueString
	ValueBool
	ValueInt
	ValueDouble
)

// AnyValue is OTLP's variant type for attribute values.
//
// The generated code modelled protobuf's oneof as an interface plus one wrapper
// struct per case, which is why reading a single attribute needed a type switch
// over four exported types. A tagged struct expresses the same thing and costs
// one field.
type AnyValue struct {
	Kind   ValueKind
	Str    string
	Bool   bool
	Int    int64
	Double float64
}

// String renders a value for the Envelope's attribute map.
//
// Array, kvlist and bytes values deliberately render as empty: the previous
// protobuf implementation's type switch had no case for them and fell through
// to "", and changing that here would alter emitted telemetry rather than just
// swap a decoder.
func (v *AnyValue) String() string {
	if v == nil {
		return ""
	}
	switch v.Kind {
	case ValueString:
		return v.Str
	case ValueInt:
		return strconv.FormatInt(v.Int, 10)
	case ValueDouble:
		return strconv.FormatFloat(v.Double, 'f', -1, 64)
	case ValueBool:
		return strconv.FormatBool(v.Bool)
	default:
		return ""
	}
}

type KeyValue struct {
	Key   string
	Value *AnyValue
}

type Resource struct {
	Attributes []*KeyValue
}

type InstrumentationScope struct {
	Name    string
	Version string
}

type Status struct {
	Message string
	Code    int32
}

type Span struct {
	TraceID           []byte
	SpanID            []byte
	ParentSpanID      []byte
	Name              string
	Kind              int32
	StartTimeUnixNano uint64
	EndTimeUnixNano   uint64
	Attributes        []*KeyValue
	Status            *Status
}

type ScopeSpans struct {
	Scope *InstrumentationScope
	Spans []*Span
}

type ResourceSpans struct {
	Resource   *Resource
	ScopeSpans []*ScopeSpans
}

type ExportTraceServiceRequest struct {
	ResourceSpans []*ResourceSpans
}

// UnmarshalExportTraceServiceRequest decodes an OTLP/HTTP binary trace export.
// It is the sole entry point this package exposes to the receiver.
func UnmarshalExportTraceServiceRequest(b []byte) (*ExportTraceServiceRequest, error) {
	req := &ExportTraceServiceRequest{}
	r := newReader(b, 0)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field == 1 && wire == wireBytes { // repeated ResourceSpans resource_spans
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			rs, err := decodeResourceSpans(sub)
			if err != nil {
				return nil, err
			}
			req.ResourceSpans = append(req.ResourceSpans, rs)
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return req, nil
}

// MarshalEmptyExportTraceServiceResponse returns the encoding of an empty
// ExportTraceServiceResponse.
//
// An OTLP success response carries no fields, and a protobuf message with no
// fields set encodes to zero bytes — verified byte-for-byte against the
// previous implementation's output. Part of what the protobuf runtime was
// being linked in to do was produce this empty slice.
func MarshalEmptyExportTraceServiceResponse() []byte { return []byte{} }

func decodeResourceSpans(r *reader) (*ResourceSpans, error) {
	out := &ResourceSpans{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wire == wireBytes {
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			switch field {
			case 1: // Resource resource
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				if out.Resource, err = decodeResource(sub); err != nil {
					return nil, err
				}
			case 2: // repeated ScopeSpans scope_spans
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				ss, err := decodeScopeSpans(sub)
				if err != nil {
					return nil, err
				}
				out.ScopeSpans = append(out.ScopeSpans, ss)
			}
			continue // unused length-delimited fields (schema_url) land here
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeResource(r *reader) (*Resource, error) {
	out := &Resource{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field == 1 && wire == wireBytes { // repeated KeyValue attributes
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			kv, err := decodeKeyValue(sub)
			if err != nil {
				return nil, err
			}
			out.Attributes = append(out.Attributes, kv)
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeScopeSpans(r *reader) (*ScopeSpans, error) {
	out := &ScopeSpans{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wire == wireBytes {
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			switch field {
			case 1: // InstrumentationScope scope
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				if out.Scope, err = decodeScope(sub); err != nil {
					return nil, err
				}
			case 2: // repeated Span spans
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				sp, err := decodeSpan(sub)
				if err != nil {
					return nil, err
				}
				out.Spans = append(out.Spans, sp)
			}
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeScope(r *reader) (*InstrumentationScope, error) {
	out := &InstrumentationScope{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wire == wireBytes {
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			switch field {
			case 1: // string name
				out.Name = string(raw)
			case 2: // string version
				out.Version = string(raw)
			}
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeSpan(r *reader) (*Span, error) {
	out := &Span{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		switch {
		case wire == wireBytes:
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			switch field {
			case 1: // bytes trace_id
				out.TraceID = clone(raw)
			case 2: // bytes span_id
				out.SpanID = clone(raw)
			case 4: // bytes parent_span_id
				out.ParentSpanID = clone(raw)
			case 5: // string name
				out.Name = string(raw)
			case 9: // repeated KeyValue attributes
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				kv, err := decodeKeyValue(sub)
				if err != nil {
					return nil, err
				}
				out.Attributes = append(out.Attributes, kv)
			case 15: // Status status
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				if out.Status, err = decodeStatus(sub); err != nil {
					return nil, err
				}
			}
		case field == 6 && wire == wireVarint: // SpanKind kind
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			out.Kind = int32(v)
		case field == 7 && wire == wireFixed64: // fixed64 start_time_unix_nano
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.StartTimeUnixNano = v
		case field == 8 && wire == wireFixed64: // fixed64 end_time_unix_nano
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.EndTimeUnixNano = v
		default:
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func decodeStatus(r *reader) (*Status, error) {
	out := &Status{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		switch {
		case field == 2 && wire == wireBytes: // string message
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			out.Message = string(raw)
		case field == 3 && wire == wireVarint: // StatusCode code
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			out.Code = int32(v)
		default:
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func decodeKeyValue(r *reader) (*KeyValue, error) {
	out := &KeyValue{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wire == wireBytes {
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			switch field {
			case 1: // string key
				out.Key = string(raw)
			case 2: // AnyValue value
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				if out.Value, err = decodeAnyValue(sub); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeAnyValue(r *reader) (*AnyValue, error) {
	out := &AnyValue{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		switch {
		case field == 1 && wire == wireBytes: // string string_value
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			out.Kind, out.Str = ValueString, string(raw)
		case field == 2 && wire == wireVarint: // bool bool_value
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			out.Kind, out.Bool = ValueBool, v != 0
		case field == 3 && wire == wireVarint: // int64 int_value
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			// int64 (not sint64) is encoded as a plain two's-complement
			// varint, so a negative value arrives as its full 64-bit pattern
			// in ten bytes; this conversion is what turns it back.
			out.Kind, out.Int = ValueInt, int64(v)
		case field == 4 && wire == wireFixed64: // double double_value
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.Kind, out.Double = ValueDouble, float64from(v)
		default:
			// Array, kvlist and bytes values land here and leave Kind as
			// ValueEmpty, matching the previous behaviour of rendering "".
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// clone copies a subslice out of the request buffer. The reader's byte results
// alias the caller's input, and span IDs outlive the decode call.
func clone(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
