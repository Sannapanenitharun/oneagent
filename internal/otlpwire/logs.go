package otlpwire

// The OTLP logs message types, carrying only the fields this agent reads.
//
// Field numbers come from opentelemetry-proto (logs/v1/logs.proto,
// collector/logs/v1/logs_service.proto). As in trace.go they are the frozen
// wire contract and safe to hard-code; the names beside them are for the
// reader. Everything the agent does not use — dropped counts, schema_url,
// flags, observed_time_unix_nano — is skipped on the wire rather than decoded
// into memory nobody reads.
//
// The message tree mirrors the trace side exactly (request → resource → scope
// → record), which is why this file reuses decodeResource, decodeScope,
// decodeKeyValue and decodeAnyValue rather than restating them.

// LogRecord is one emitted log line.
type LogRecord struct {
	TimeUnixNano uint64
	// SeverityNumber is OTLP's numeric severity (1..24). Kept alongside
	// SeverityText because the text is free-form and frequently absent, while
	// the number is well-defined and is what a severity filter can rely on.
	SeverityNumber int32
	SeverityText   string
	Body           *AnyValue
	Attributes     []*KeyValue
	// TraceID and SpanID are set when the log was emitted inside an active
	// span. They are what connects an application's logs to its traces, so
	// they are decoded even though nothing else here needs them.
	TraceID []byte
	SpanID  []byte
}

type ScopeLogs struct {
	Scope      *InstrumentationScope
	LogRecords []*LogRecord
}

type ResourceLogs struct {
	Resource  *Resource
	ScopeLogs []*ScopeLogs
}

type ExportLogsServiceRequest struct {
	ResourceLogs []*ResourceLogs
}

// UnmarshalExportLogsServiceRequest decodes an OTLP/HTTP binary log export.
func UnmarshalExportLogsServiceRequest(b []byte) (*ExportLogsServiceRequest, error) {
	req := &ExportLogsServiceRequest{}
	r := newReader(b, 0)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field == 1 && wire == wireBytes { // repeated ResourceLogs resource_logs
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			rl, err := decodeResourceLogs(sub)
			if err != nil {
				return nil, err
			}
			req.ResourceLogs = append(req.ResourceLogs, rl)
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return req, nil
}

// MarshalEmptyExportLogsServiceResponse returns the encoding of an empty
// ExportLogsServiceResponse — no fields set, so zero bytes. Same reasoning as
// the trace response it sits beside.
func MarshalEmptyExportLogsServiceResponse() []byte { return []byte{} }

func decodeResourceLogs(r *reader) (*ResourceLogs, error) {
	out := &ResourceLogs{}
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
			case 2: // repeated ScopeLogs scope_logs
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				sl, err := decodeScopeLogs(sub)
				if err != nil {
					return nil, err
				}
				out.ScopeLogs = append(out.ScopeLogs, sl)
			}
			continue // unused length-delimited fields (schema_url) land here
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeScopeLogs(r *reader) (*ScopeLogs, error) {
	out := &ScopeLogs{}
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
			case 2: // repeated LogRecord log_records
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				lr, err := decodeLogRecord(sub)
				if err != nil {
					return nil, err
				}
				out.LogRecords = append(out.LogRecords, lr)
			}
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeLogRecord(r *reader) (*LogRecord, error) {
	out := &LogRecord{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		switch {
		case field == 1 && wire == wireFixed64: // fixed64 time_unix_nano
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.TimeUnixNano = v
		case field == 2 && wire == wireVarint: // SeverityNumber severity_number
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			out.SeverityNumber = int32(v)
		case field == 11 && wire == wireFixed64: // fixed64 observed_time_unix_nano
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			// Only as a fallback. A record with no time_unix_nano is legal and
			// the SDKs do emit them; observed time is the collector's own
			// reading and is the closest thing available. Taken here rather
			// than at use so field order on the wire cannot decide which wins.
			if out.TimeUnixNano == 0 {
				out.TimeUnixNano = v
			}
		case wire == wireBytes:
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			switch field {
			case 3: // string severity_text
				out.SeverityText = string(raw)
			case 5: // AnyValue body
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				if out.Body, err = decodeAnyValue(sub); err != nil {
					return nil, err
				}
			case 6: // repeated KeyValue attributes
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				kv, err := decodeKeyValue(sub)
				if err != nil {
					return nil, err
				}
				out.Attributes = append(out.Attributes, kv)
			case 9: // bytes trace_id
				out.TraceID = append([]byte(nil), raw...)
			case 10: // bytes span_id
				out.SpanID = append([]byte(nil), raw...)
			}
		default:
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
