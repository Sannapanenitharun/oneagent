package otlpwire

import "math"

// The OTLP metrics message types, carrying only the fields this agent reads.
//
// Field numbers come from opentelemetry-proto (metrics/v1/metrics.proto,
// collector/metrics/v1/metrics_service.proto), and as elsewhere in this
// package they are the frozen wire contract.
//
// Three of OTLP's five metric types are decoded: gauge, sum and histogram.
// They are what SDK auto-instrumentation actually emits — a request counter is
// a sum, a queue depth or a memory reading is a gauge, and http.server.duration
// is a histogram. The two that are skipped are skipped deliberately:
//
//   - summary is a legacy type OTLP keeps only for Prometheus compatibility;
//     nothing in an OTel SDK produces it.
//   - exponential_histogram is produced only by explicitly configuring an
//     exponential aggregation, and decoding it would mean deciding how its
//     buckets map onto the agent's own histogram representation. That is a
//     real design question, not an omission to be filled in silently.
//
// A metric of a skipped type decodes to a Metric with Kind MetricUnknown and
// no points, so the receiver drops it rather than mistaking it for an empty
// gauge.

// MetricKind discriminates the Metric.data oneof.
type MetricKind uint8

const (
	MetricUnknown MetricKind = iota
	MetricGauge
	MetricSum
	MetricHistogram
)

// NumberDataPoint is one reading of a gauge or a sum.
type NumberDataPoint struct {
	Attributes   []*KeyValue
	TimeUnixNano uint64
	Value        float64
}

// HistogramDataPoint is one window of a distribution.
//
// Only count and sum are decoded, not the buckets. Those two are enough for
// the rate and the mean, which is what the agent's own metric shape can carry;
// bucket counts would need a histogram-aware path all the way through the
// exporter, and inventing quantiles from them here would be worse than not
// offering them.
type HistogramDataPoint struct {
	Attributes   []*KeyValue
	TimeUnixNano uint64
	Count        uint64
	Sum          float64
	// HasSum distinguishes "the sum is genuinely 0" from "no sum was sent".
	// It is an optional field in the proto precisely because a histogram of
	// values that are all zero is meaningful.
	HasSum bool
}

type Metric struct {
	Name string
	Unit string
	Kind MetricKind
	// IsMonotonic is meaningful only for a sum, where it separates a counter
	// that only rises — which a consumer may turn into a rate — from an
	// up-down counter, where a drop is real data rather than a reset.
	IsMonotonic     bool
	NumberPoints    []*NumberDataPoint
	HistogramPoints []*HistogramDataPoint
}

type ScopeMetrics struct {
	Scope   *InstrumentationScope
	Metrics []*Metric
}

type ResourceMetrics struct {
	Resource     *Resource
	ScopeMetrics []*ScopeMetrics
}

type ExportMetricsServiceRequest struct {
	ResourceMetrics []*ResourceMetrics
}

// UnmarshalExportMetricsServiceRequest decodes an OTLP/HTTP binary metric export.
func UnmarshalExportMetricsServiceRequest(b []byte) (*ExportMetricsServiceRequest, error) {
	req := &ExportMetricsServiceRequest{}
	r := newReader(b, 0)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field == 1 && wire == wireBytes { // repeated ResourceMetrics resource_metrics
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			rm, err := decodeResourceMetrics(sub)
			if err != nil {
				return nil, err
			}
			req.ResourceMetrics = append(req.ResourceMetrics, rm)
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return req, nil
}

// MarshalEmptyExportMetricsServiceResponse returns the encoding of an empty
// ExportMetricsServiceResponse — no fields set, so zero bytes.
func MarshalEmptyExportMetricsServiceResponse() []byte { return []byte{} }

func decodeResourceMetrics(r *reader) (*ResourceMetrics, error) {
	out := &ResourceMetrics{}
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
			case 2: // repeated ScopeMetrics scope_metrics
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				sm, err := decodeScopeMetrics(sub)
				if err != nil {
					return nil, err
				}
				out.ScopeMetrics = append(out.ScopeMetrics, sm)
			}
			continue // unused length-delimited fields (schema_url) land here
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeScopeMetrics(r *reader) (*ScopeMetrics, error) {
	out := &ScopeMetrics{}
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
			case 2: // repeated Metric metrics
				sub, err := r.sub(raw)
				if err != nil {
					return nil, err
				}
				m, err := decodeMetric(sub)
				if err != nil {
					return nil, err
				}
				out.Metrics = append(out.Metrics, m)
			}
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeMetric(r *reader) (*Metric, error) {
	out := &Metric{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		raw, err := r.bytes()
		if err != nil {
			return nil, err
		}
		switch field {
		case 1: // string name
			out.Name = string(raw)
		case 3: // string unit
			out.Unit = string(raw)
		case 5: // Gauge gauge
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			pts, _, err := decodeNumberDataPoints(sub)
			if err != nil {
				return nil, err
			}
			out.Kind, out.NumberPoints = MetricGauge, pts
		case 7: // Sum sum
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			pts, monotonic, err := decodeNumberDataPoints(sub)
			if err != nil {
				return nil, err
			}
			out.Kind, out.NumberPoints, out.IsMonotonic = MetricSum, pts, monotonic
		case 9: // Histogram histogram
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			pts, err := decodeHistogramDataPoints(sub)
			if err != nil {
				return nil, err
			}
			out.Kind, out.HistogramPoints = MetricHistogram, pts
		}
		// description, exponential_histogram, summary and metadata land here
		// and are discarded — see the note at the top of the file.
	}
	return out, nil
}

// decodeNumberDataPoints reads a Gauge or a Sum. Both wrap the same repeated
// NumberDataPoint in field 1; only Sum carries is_monotonic, which comes back
// false for a Gauge because the field is absent there rather than false.
func decodeNumberDataPoints(r *reader) ([]*NumberDataPoint, bool, error) {
	var pts []*NumberDataPoint
	monotonic := false
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, false, err
		}
		if field == 3 && wire == wireVarint { // bool is_monotonic
			v, err := r.varint()
			if err != nil {
				return nil, false, err
			}
			monotonic = v != 0
			continue
		}
		if field == 1 && wire == wireBytes { // repeated NumberDataPoint data_points
			raw, err := r.bytes()
			if err != nil {
				return nil, false, err
			}
			sub, err := r.sub(raw)
			if err != nil {
				return nil, false, err
			}
			p, err := decodeNumberDataPoint(sub)
			if err != nil {
				return nil, false, err
			}
			pts = append(pts, p)
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, false, err
		}
	}
	return pts, monotonic, nil
}

func decodeNumberDataPoint(r *reader) (*NumberDataPoint, error) {
	out := &NumberDataPoint{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		switch {
		case field == 3 && wire == wireFixed64: // fixed64 time_unix_nano
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.TimeUnixNano = v
		case field == 4 && wire == wireFixed64: // double as_double
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.Value = math.Float64frombits(v)
		case field == 6 && wire == wireFixed64: // sfixed64 as_int
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			// sfixed64 is plain two's complement, not zigzag — that encoding
			// belongs to sint64. Reinterpreting the bits is the whole
			// conversion; going through zigzag here would turn every negative
			// reading into a large positive one.
			out.Value = float64(int64(v))
		case field == 7 && wire == wireBytes: // repeated KeyValue attributes
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
		default:
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func decodeHistogramDataPoints(r *reader) ([]*HistogramDataPoint, error) {
	var pts []*HistogramDataPoint
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field == 1 && wire == wireBytes { // repeated HistogramDataPoint data_points
			raw, err := r.bytes()
			if err != nil {
				return nil, err
			}
			sub, err := r.sub(raw)
			if err != nil {
				return nil, err
			}
			p, err := decodeHistogramDataPoint(sub)
			if err != nil {
				return nil, err
			}
			pts = append(pts, p)
			continue
		}
		if err := r.skip(wire); err != nil {
			return nil, err
		}
	}
	return pts, nil
}

func decodeHistogramDataPoint(r *reader) (*HistogramDataPoint, error) {
	out := &HistogramDataPoint{}
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		switch {
		case field == 3 && wire == wireFixed64: // fixed64 time_unix_nano
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.TimeUnixNano = v
		case field == 4 && wire == wireFixed64: // fixed64 count
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.Count = v
		case field == 5 && wire == wireFixed64: // optional double sum
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			out.Sum, out.HasSum = math.Float64frombits(v), true
		case field == 9 && wire == wireBytes: // repeated KeyValue attributes
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
		default:
			// bucket_counts and explicit_bounds land here. They arrive packed,
			// which skip handles as a single length-delimited field.
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
