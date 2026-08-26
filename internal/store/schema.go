package store

// The schema.
//
// Three signal tables plus one host table, applied at startup and safe to run
// repeatedly. Migrations are IF NOT EXISTS rather than a versioned ladder: at
// this stage every change is additive, and a ladder that has never had to
// migrate anything is a ladder nobody has tested.
//
// # Why the layout is what it is
//
// Every table is ORDER BY (something low-cardinality, ..., timestamp). That
// ordering key is also the primary index in ClickHouse, so it decides which
// queries read a few granules and which read the partition. The queries this
// backend exists to serve all name a signal and a host or service first and a
// time range second, so that is the order.
//
// PARTITION BY toDate(timestamp) keeps a day's data together, which is what
// makes TTL deletion a partition drop rather than a rewrite, and what lets a
// query for "the last 15 minutes" skip every other day without reading them.
// Finer partitioning (by hour) would multiply the part count for no gain at
// this size; coarser (by month) would make retention expensive.
//
// Attributes ride in a Map(LowCardinality(String), String) rather than being
// flattened into columns. Flattening is faster to query and impossible to do
// here: the agent forwards whatever an instrumented application chose to
// attach, so the column set is not knowable in advance. The map keeps the
// schema fixed while the data stays open, and LowCardinality on the key side
// costs almost nothing because attribute NAMES repeat even when their values
// do not.
//
// Codecs are chosen per column rather than left to the default. Delta+ZSTD on
// timestamps exploits that they arrive nearly sorted; Gorilla on float values
// exploits that consecutive samples of the same series differ in few bits.
// Both are the standard choices for this shape of data and both matter more
// than anything else here at volume.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS metrics (
    timestamp    DateTime64(3)                          CODEC(Delta, ZSTD(1)),
    name         LowCardinality(String),
    host_id      LowCardinality(String),
    service      LowCardinality(String),
    value        Float64                                CODEC(Gorilla, ZSTD(1)),
    -- Cumulative counters and gauges answer different questions and must not
    -- be averaged together, so which one this is travels with the point.
    is_monotonic UInt8                                  CODEC(ZSTD(1)),
    attributes   Map(LowCardinality(String), String)
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (name, host_id, timestamp)
TTL toDateTime(timestamp) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192
`

const logsDDL = `
CREATE TABLE IF NOT EXISTS logs (
    timestamp   DateTime64(9)                           CODEC(Delta, ZSTD(1)),
    host_id     LowCardinality(String),
    service     LowCardinality(String),
    severity    LowCardinality(String),
    -- The numeric scale is kept alongside the name because "at least WARN" is
    -- a range query, and doing it on the text would mean encoding the
    -- ordering into every WHERE clause.
    severity_num UInt8,
    body        String                                  CODEC(ZSTD(3)),
    trace_id    String                                  CODEC(ZSTD(1)),
    span_id     String                                  CODEC(ZSTD(1)),
    attributes  Map(LowCardinality(String), String)
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (host_id, service, timestamp)
TTL toDateTime(timestamp) + INTERVAL 15 DAY
SETTINGS index_granularity = 8192
`

// Spans are ordered by service before time because the questions asked of
// traces are per-service — latency, error rate, dependencies — while lookup by
// trace_id is a needle in a haystack that the ordering key cannot help with
// anyway. The bloom filter index below is what makes that lookup fast, at a
// fraction of what a trace_id-first ordering would cost every other query.
const spansDDL = `
CREATE TABLE IF NOT EXISTS spans (
    timestamp      DateTime64(9)                        CODEC(Delta, ZSTD(1)),
    trace_id       String                               CODEC(ZSTD(1)),
    span_id        String                               CODEC(ZSTD(1)),
    parent_span_id String                               CODEC(ZSTD(1)),
    service        LowCardinality(String),
    name           LowCardinality(String),
    kind           LowCardinality(String),
    duration_ns    UInt64                               CODEC(T64, ZSTD(1)),
    status_code    LowCardinality(String),
    status_message String                               CODEC(ZSTD(1)),
    host_id        LowCardinality(String),
    attributes     Map(LowCardinality(String), String),
    INDEX idx_trace_id trace_id TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (service, timestamp)
TTL toDateTime(timestamp) + INTERVAL 15 DAY
SETTINGS index_granularity = 8192
`

// hosts is the fleet inventory: one row per machine, carrying what it last
// said about itself.
//
// It exists so the fleet view is a single small query rather than a scan over
// the metrics table. Deriving "which hosts exist" from metrics is possible and
// is what the browser-polling version effectively did; at fleet scale it means
// reading a time range of every series to answer a question about identity.
//
// ReplacingMergeTree collapses to the newest row per host_id in the
// background. Queries still use FINAL — merges are asynchronous, so without it
// a host that reported twice can appear twice — but the table is one row per
// host, which is the one size where FINAL costs nothing worth measuring.
const hostsDDL = `
CREATE TABLE IF NOT EXISTS hosts (
    host_id     String,
    agent_id    String,
    last_seen   DateTime64(3),
    first_seen  DateTime64(3),
    attributes  Map(LowCardinality(String), String)
)
ENGINE = ReplacingMergeTree(last_seen)
ORDER BY host_id
`

// migrations run in order. Splitting them keeps a failure pointing at one
// table instead of one long statement.
func migrations() []string {
	return []string{schemaDDL, logsDDL, spansDDL, hostsDDL}
}
