// Regression check for src/adapters.js — the only place in the frontend with
// real logic (tree reconstruction, service graph derivation, counter rates).
//
// Runs with plain node, no test framework and no running agent:
//   npm test
//
// The fixture below mirrors an actual /api/snapshot payload, captured from a
// live agent that had received a four-span trace across three services. Keep
// it in that shape — if the agent's payload changes, this is where it should
// break first.

import {
  deriveServices, deriveTraces, deriveEdges, layoutTopology,
  deriveInfra, deriveAllSeries, deriveLogs, deriveTraffic,
  globalStats, toRate, fmtRps,
  alignSeries, foldSmallest, hostMetricPanels, fmtBytes, fmtMetric, MAX_SERIES_PER_PANEL,
  parseLogBody, flattenFields, ADAPTER_VERSION, CONTRACT, contractMatches,
  hostRow, deriveTopologyNodes, peerName, peerType,
} from "./src/adapters.js";

const T0 = 1786800000000;

// api-gateway -> checkout -> {checkout SELECT, payments ChargeCard(error)}
const SNAP = {
  agent_id: "test-1",
  version: "v1.1.0",
  started_at: T0 - 60000,
  now: T0 + 1000,
  retain_sec: 900,
  counts: { metric: 227, trace: 4 },
  series_dropped: 0,
  series: [
    { name: "system.cpu.time", labels: { state: "idle", cpu: "cpu-total" }, cumulative: true,
      points: [{ t: T0, v: 1000 }, { t: T0 + 1000, v: 1007.2 }] },
    { name: "system.cpu.time", labels: { state: "user", cpu: "cpu-total" }, cumulative: true,
      points: [{ t: T0, v: 100 }, { t: T0 + 1000, v: 100.8 }] },
    { name: "system.memory.usage", labels: { state: "used" }, cumulative: false,
      points: [{ t: T0 + 1000, v: 6_000_000_000 }] },
    { name: "system.memory.usage", labels: { state: "free" }, cumulative: false,
      points: [{ t: T0 + 1000, v: 4_000_000_000 }] },
    { name: "system.filesystem.usage", labels: { mountpoint: "/", state: "used" }, cumulative: false,
      points: [{ t: T0 + 1000, v: 40 }] },
    { name: "system.filesystem.usage", labels: { mountpoint: "/", state: "free" }, cumulative: false,
      points: [{ t: T0 + 1000, v: 60 }] },
    { name: "system.cpu.load_average.1m", labels: null, cumulative: false,
      points: [{ t: T0 + 1000, v: 1.25 }] },
    { name: "system.network.io", labels: { device: "eth0", direction: "receive" }, cumulative: true,
      points: [{ t: T0, v: 1000 }, { t: T0 + 1000, v: 3048 }] },
  ],
  logs: [
    { t: T0, source: "/var/log/app/app.log", message: "started ok" },
    { t: T0 + 500, source: "/var/log/app/app.log", message: "ERROR upstream 503 from stripe" },
    { t: T0 + 800, source: "/var/log/app/app.log", message: "warning: retry 2/3" },
  ],
  spans: [
    { t: T0, trace_id: "tr1", span_id: "s1", service: "api-gateway", name: "POST /checkout", dur_ms: 600 },
    { t: T0 + 10, trace_id: "tr1", span_id: "s2", parent_id: "s1", service: "checkout", name: "CreateOrder", dur_ms: 570 },
    { t: T0 + 20, trace_id: "tr1", span_id: "s3", parent_id: "s2", service: "checkout", name: "SELECT orders", dur_ms: 100 },
    { t: T0 + 130, trace_id: "tr1", span_id: "s4", parent_id: "s2", service: "payments", name: "ChargeCard", dur_ms: 440, status: "2" },
  ],
};

let failed = 0;
const check = (name, cond, detail = "") => {
  if (cond) console.log(`  PASS  ${name}`);
  else { console.log(`  FAIL  ${name} ${detail}`); failed++; }
};

console.log("traces");
const [trace] = deriveTraces(SNAP);
const byOp = Object.fromEntries(trace.spans.map((s) => [s.op, s]));
check("root at depth 0", byOp["POST /checkout"].depth === 0);
check("child at depth 1", byOp["CreateOrder"].depth === 1, `got ${byOp["CreateOrder"].depth}`);
check("grandchildren at depth 2",
  byOp["SELECT orders"].depth === 2 && byOp["ChargeCard"].depth === 2);
check("root service identified", trace.root === "api-gateway");
check("error status propagates to trace", trace.status === "error");
check("span start is relative to trace start", byOp["ChargeCard"].start === 130);

// The fixture above carries no span kinds, which is what an older agent sends.
// The derivation has to fall back to parent links there rather than returning
// an empty map — a dashboard pointed at a host that has not been upgraded must
// still draw its topology.
console.log("edges (no span kinds — fallback path)");
const edges = deriveEdges(SNAP);
const has = (f, t) => edges.some((e) => e.from === f && e.to === t);
check("api-gateway -> checkout", has("api-gateway", "checkout"));
check("checkout -> payments", has("checkout", "payments"));
check("self-calls are not dependencies", !has("checkout", "checkout"));

const payEdge = edges.find((e) => e.to === "payments");
check("edges carry their traffic", payEdge.calls === 1, `calls=${payEdge?.calls}`);
check("edges carry their failures", payEdge.errors === 1 && payEdge.errPct === 100,
  `errors=${payEdge?.errors} errPct=${payEdge?.errPct}`);
check("edges carry latency", payEdge.p99 > 0, `p99=${payEdge?.p99}`);
check("nothing is inferred without kinds", edges.every((e) => !e.virtual));

// A service graph is a client span paired with a server span in another
// service. With kinds present the derivation must use them, and must find the
// dependencies that have no span of their own at all.
console.log("edges (span kinds + uninstrumented peers)");
const KINDED = {
  agent_id: "x", version: "v", started_at: T0, now: T0 + 1000, retain_sec: 900,
  counts: {}, series: [], logs: [],
  spans: [
    // gateway -> orders, paired client/server, called twice with one failure
    { t: T0, trace_id: "t1", span_id: "a1", service: "gateway", name: "GET /o", kind: "client", dur_ms: 90 },
    { t: T0, trace_id: "t1", span_id: "b1", parent_id: "a1", service: "orders", name: "GET /o", kind: "server", dur_ms: 70 },
    { t: T0, trace_id: "t2", span_id: "a2", service: "gateway", name: "GET /o", kind: "client", dur_ms: 110 },
    { t: T0, trace_id: "t2", span_id: "b2", parent_id: "a2", service: "orders", name: "GET /o", kind: "server", dur_ms: 95, status: "2" },
    // orders -> postgres: a client span with no server span anywhere
    { t: T0, trace_id: "t1", span_id: "c1", parent_id: "b1", service: "orders", name: "SELECT", kind: "client",
      dur_ms: 20, peer: { "db.system": "postgresql", "db.name": "ordersdb" } },
    // orders -> kafka: a producer with no consumer in this window
    { t: T0, trace_id: "t1", span_id: "d1", parent_id: "b1", service: "orders", name: "publish", kind: "producer",
      dur_ms: 5, peer: { "messaging.system": "kafka", "messaging.destination.name": "order-events" } },
    // orders -> gateway named by peer.service, whose spans ARE present:
    // outside the window this would look uninstrumented, and must not.
    { t: T0, trace_id: "t3", span_id: "e1", service: "orders", name: "callback", kind: "client",
      dur_ms: 15, peer: { "peer.service": "gateway" } },
  ],
};
const ke = deriveEdges(KINDED);
const kf = (f, t) => ke.find((e) => e.from === f && e.to === t);

check("client/server pair becomes an edge", !!kf("gateway", "orders"));
check("repeat calls aggregate onto one edge", kf("gateway", "orders").calls === 2,
  `calls=${kf("gateway", "orders")?.calls}`);
check("either side failing fails the call", kf("gateway", "orders").errors === 1);
check("paired edges are not inferred", kf("gateway", "orders").virtual === false);
// The caller's duration, not the callee's: it is what the caller experienced.
check("edge latency is the caller's view", kf("gateway", "orders").p99 === 110,
  `p99=${kf("gateway", "orders")?.p99}`);

check("uninstrumented database becomes an edge", !!kf("orders", "ordersdb"));
check("database edge is marked inferred", kf("orders", "ordersdb").virtual === true);
check("database edge is typed", kf("orders", "ordersdb").type === "database",
  `type=${kf("orders", "ordersdb")?.type}`);
check("uninstrumented queue becomes an edge", !!kf("orders", "order-events"));
check("queue edge is typed", kf("orders", "order-events").type === "messaging");

// The rolling span buffer means a real service's own spans can age out first.
// Treating it as uninstrumented would make it flicker into a separate node.
check("a peer that is a known service is not inferred", kf("orders", "gateway")?.virtual === false,
  `virtual=${kf("orders", "gateway")?.virtual}`);

// Internal work is not a dependency.
check("no self edges", ke.every((e) => e.from !== e.to));

console.log("topology nodes");
const kNodes = deriveTopologyNodes(deriveServices(KINDED), ke);
const nodeIds = kNodes.map((n) => n.id);
check("instrumented services are nodes", nodeIds.includes("gateway") && nodeIds.includes("orders"));
check("inferred dependencies are nodes", nodeIds.includes("ordersdb") && nodeIds.includes("order-events"));
check("no duplicate nodes", new Set(nodeIds).size === nodeIds.length);
check("inferred nodes are marked", kNodes.find((n) => n.id === "ordersdb").virtual === true);
check("real services are not marked inferred", kNodes.find((n) => n.id === "orders").virtual === false);
// Inferred nodes must not be counted as services anywhere else, or "healthy /
// seen" starts counting databases nobody instrumented.
check("inferred nodes stay out of deriveServices",
  !deriveServices(KINDED).some((s) => s.id === "ordersdb"));

console.log("peer naming");
check("peer.service wins", peerName({ "peer.service": "billing", "db.name": "x" }) === "billing");
check("db name beats db system", peerName({ "db.system": "postgresql", "db.name": "ordersdb" }) === "ordersdb");
check("falls back to the address", peerName({ "server.address": "api.stripe.com" }) === "api.stripe.com");
check("no peer means no name", peerName(undefined) === "" && peerName({}) === "");
check("database is typed", peerType({ "db.system": "redis" }) === "database");
check("messaging is typed", peerType({ "messaging.system": "kafka" }) === "messaging");
check("plain http is a service", peerType({ "server.address": "x" }) === "service");

console.log("services");
const services = deriveServices(SNAP);
check("one entry per service", services.length === 3, `got ${services.length}`);
check("error rate computed", services.find((s) => s.id === "payments").err === 100);
check("slow service marked degraded", services.every((s) => s.status === "degraded"));

console.log("counter rates");
// 2048 bytes over 1s, from a cumulative counter.
const rate = toRate(SNAP.series.find((s) => s.name === "system.network.io").points);
check("cumulative differentiated to per-second", rate[0].v === 2048, `got ${rate[0]?.v}`);
check("counter reset drops the interval",
  toRate([{ t: T0, v: 100 }, { t: T0 + 1000, v: 5 }]).length === 0);

console.log("host");
const [host] = deriveInfra(SNAP);
// idle 7.2/s of 8.0/s total => 10% busy
check("cpu busy derived from idle share", host.cpu === 10, `got ${host.cpu}`);
check("memory percentage derived", host.mem === 60, `got ${host.mem}`);
check("worst mountpoint reported", host.disk === 40, `got ${host.disk}`);

console.log("logs");
const logs = deriveLogs(SNAP);
check("newest line first", logs[0].msg.includes("retry"));
check("ERROR classified", logs.find((l) => l.msg.includes("503")).lvl === "ERROR");
check("lowercase warning classified", logs[0].lvl === "WARN", `got ${logs[0].lvl}`);
check("source reduced to basename", logs[0].svc === "app.log");

console.log("formatting");
check("low rate keeps precision", fmtRps(0.004) === "0.004");
check("mid rate one decimal", fmtRps(4.25) === "4.3");
check("high rate rounded", fmtRps(842.4) === "842");
check("zero stays zero", fmtRps(0) === "0");

console.log("log body parsing");
{
  // The real shape from a MongoDB log line: whole line is one JSON object with
  // nested attr/message documents.
  const mongo = '{"t":{"$date":"2026-08-15T20:07:47.305+00:00"},"s":"I","c":"WTCHKPT","id":22430,' +
    '"ctx":"Checkpointer","msg":"WiredTiger message","attr":{"message":{"ts_sec":1786824467,' +
    '"session_name":"WT_SESSION.checkpoint","verbose_level":"DEBUG_1"}}}';
  const p = parseLogBody(mongo);
  check("whole-line JSON is parsed", p !== null && p.value.c === "WTCHKPT");
  check("no prefix when the line is pure JSON", p.prefix === "");

  const fields = flattenFields(p.value);
  const byPath = Object.fromEntries(fields.map((f) => [f.path, f.value]));
  check("nested documents flatten to dotted paths", byPath["t.$date"] === "2026-08-15T20:07:47.305+00:00");
  check("deeply nested values survive", byPath["attr.message.session_name"] === "WT_SESSION.checkpoint");
  check("scalars keep their type", byPath["id"] === 22430);
  check("container keys are not emitted as fields", !("attr" in byPath) && !("t" in byPath));
}
{
  // Plain-text prefix then JSON — the other common structured-logger shape.
  const p = parseLogBody('2026-08-15 12:00:00 INFO {"user":"ubuntu","ok":true}');
  check("prefix before JSON is separated", p !== null && p.prefix === "2026-08-15 12:00:00 INFO");
  check("body after a prefix is parsed", p.value.user === "ubuntu");
}
{
  // Everything that must NOT be treated as structured, because guessing at
  // partial structure would invent fields that were never logged.
  const syslog = "2026-08-15T21:38:01+00:00 ip-172-31-33-81 CRON[6748]: session closed for user ubuntu";
  check("plain syslog is not structured", parseLogBody(syslog) === null);
  check("empty line is safe", parseLogBody("") === null);
  check("unterminated JSON is rejected", parseLogBody('{"a":1') === null);
  check("trailing text after JSON is rejected", parseLogBody('{"a":1} and then more') === null);
  check("a bare array is not a field tree", parseLogBody("[1,2,3]") === null);
  check("a brace in prose does not parse", parseLogBody("cannot open {file}") === null);
}
{
  // deriveLogs must carry what the detail view needs.
  const snap = { logs: [{ t: T0, source: "/var/log/mongod.log", message: '{"s":"E","msg":"boom"}', labels: { host: "test-1" } }] };
  const [l] = deriveLogs(snap);
  check("full source path retained", l.src === "/var/log/mongod.log");
  check("basename still shown", l.svc === "mongod.log");
  check("raw timestamp retained", l.tms === T0);
  check("labels retained", l.labels.host === "test-1");
  check("structured body attached", l.structured.value.msg === "boom");
}

console.log("adapter versioning + severity provenance");
check("adapter version is a non-empty string", typeof ADAPTER_VERSION === "string" && ADAPTER_VERSION.length > 0);
check("contract is a non-empty string", typeof CONTRACT === "string" && CONTRACT.length > 0);
check("matching contract accepted", contractMatches({ adapter_contract: CONTRACT }));
// An agent older than the field is readable, just unlabelled — refusing it
// would break the UI against every agent that has not been upgraded yet.
check("absent contract is tolerated", contractMatches({}) && contractMatches(null));
check("mismatched contract is reported", !contractMatches({ adapter_contract: "999" }));
{
  const logs = deriveLogs(SNAP);
  check("every log line carries its severity provenance",
    logs.length > 0 && logs.every((l) => l.lvlSource === `client:${ADAPTER_VERSION}`));
  // Item 5 is metadata only: the classification itself must be untouched.
  check("classification unchanged — ERROR still ERROR",
    logs.find((l) => l.msg.includes("503")).lvl === "ERROR");
  check("classification unchanged — lowercase warning still WARN", logs[0].lvl === "WARN");
}

console.log("host metric panels");
{
  // A gap in one series must stay a gap. Filling it with 0 on an errors chart
  // states "no errors happened", which is a different claim from "we did not
  // sample here" — and the one a reader would act on.
  const { rows, keys } = alignSeries([
    { key: "a", points: [{ t: 1, v: 10 }, { t: 2, v: 20 }] },
    { key: "b", points: [{ t: 2, v: 5 }] },
  ]);
  check("aligned on the union of timestamps", rows.length === 2);
  check("missing sample is null, not zero", rows[0].b === null);
  check("present sample kept", rows[1].b === 5);
  check("keys preserved", keys.join(",") === "a,b");
}
{
  // Ranked by peak, not by last value: a device that spiked and settled is
  // the interesting one, and last-value ranking would drop it.
  const many = [
    { key: "spiky", points: [{ t: 1, v: 900 }, { t: 2, v: 0 }] },
    ...["a", "b", "c", "d", "e", "f"].map((k, i) => ({ key: k, points: [{ t: 1, v: i }, { t: 2, v: i }] })),
  ];
  const folded = foldSmallest(many);
  check("folded to the palette limit", folded.length === MAX_SERIES_PER_PANEL);
  check("peak series survives folding", folded.some((s) => s.key === "spiky"));
  check("remainder becomes one labelled series", folded[folded.length - 1].key.startsWith("other ("));
  // 7 series in, 5 kept by peak, so "other" is the 2 lowest: peaks 0 and 1.
  const other = folded[folded.length - 1];
  check("remainder is summed, not dropped", other.key === "other (2)" && other.points[0].v === 1);
  check("under the limit is left alone", foldSmallest(many.slice(0, 3)).length === 3);
}
{
  // system.cpu.time is a cumulative per-state counter; its rates sum to the
  // core count, so the panel must divide by that total to read as a percentage
  // regardless of how many cores the host has.
  // Three samples, because a cumulative counter yields its first rate only on
  // the second one — and a single point is not drawable as a line.
  // Per 2s interval: idle accrues 3s, user 1s. That is 2 cores' worth of time,
  // and the panel must still read 75%/25% rather than 150%/50%.
  const cpuSnap = { series: ["idle", "user"].map((state) => ({
    name: "system.cpu.time", cumulative: true, labels: { state, cpu: "cpu-total" },
    points: [0, 1, 2].map((n) => ({ t: n * 2000, v: n * (state === "idle" ? 3 : 1) })),
  })) };
  const cpu = hostMetricPanels(cpuSnap).find((p) => p.id === "cpu");
  check("cpu panel is drawable", cpu.rows.length === 2);
  check("cpu is normalised to a percentage", Math.round(cpu.rows[0].idle) === 75);
  check("cpu states sum to 100", Math.round(cpu.rows[0].idle + cpu.rows[0].user) === 100);
}
{
  const panels = hostMetricPanels({ series: [] });
  check("every panel present on an empty agent", panels.length === 13);
  check("empty panels name what they need", panels.every((p) => p.needs && p.series.length === 0));
  check("empty panels are safe to render", panels.every((p) => Array.isArray(p.rows) && Array.isArray(p.keys)));
}
check("bytes use binary units", fmtBytes(1536) === "1.5 KiB");
check("byte rates carry the suffix", fmtBytes(1024 * 1024, true) === "1.0 MiB/s");
check("percent keeps one decimal", fmtMetric(12.34, "%") === "12.3%");
check("seconds-per-second read as ms", fmtMetric(0.0125, "s/s") === "12.5 ms/s");
check("null renders as a dash", fmtMetric(null, "") === "—");

console.log("empty snapshot (agent just started)");
const EMPTY = { agent_id: "x", version: "v", started_at: T0, now: T0, retain_sec: 900, counts: {}, series: [], logs: [], spans: [] };
check("traces safe", deriveTraces(EMPTY).length === 0);
check("edges safe", deriveEdges(EMPTY).length === 0);
check("services safe", deriveServices(EMPTY).length === 0);
check("infra safe", deriveInfra(EMPTY).length === 0);
check("layout safe", Object.keys(layoutTopology([], [])).length === 0);
check("series safe", deriveAllSeries(EMPTY).length === 0);
check("traffic safe", deriveTraffic(EMPTY).rps.length === 0);
check("null snapshot safe", globalStats(null).envelopes === 0);

// hostRow feeds the fleet table, which is the only view that shows many
// machines at once — so it is the one place where confusing two hosts is
// possible, and where identity has to survive the reduction intact.
console.log("host row");
const EC2 = {
  ...EMPTY,
  agent_id: "prod-web-01",
  host: {
    "cloud.provider": "aws",
    "cloud.account.id": "123456789012",
    "cloud.region": "us-east-1",
    "cloud.availability_zone": "us-east-1a",
    "host.id": "i-0123456789abcdef0",
    "host.type": "t3.medium",
    "host.image.id": "ami-0abcdef1234567890",
  },
  series: SNAP.series,
};
const ec2Row = hostRow(EC2);
check("carries the instance id", ec2Row.instanceID === "i-0123456789abcdef0", ec2Row.instanceID);
check("carries the instance type", ec2Row.instanceType === "t3.medium", ec2Row.instanceType);
check("prefers the AZ over the region", ec2Row.zone === "us-east-1a", ec2Row.zone);
check("keeps agent_id as the host name", ec2Row.host === "prod-web-01", ec2Row.host);

// An instance reporting a region but no AZ still gets a usable zone column.
const REGION_ONLY = { ...EC2, host: { "cloud.region": "eu-west-2", "host.id": "i-abc" } };
check("falls back to the region", hostRow(REGION_ONLY).zone === "eu-west-2");

// Off a cloud host the agent omits `host` entirely. These must come back as
// strings: the fleet table sorts them with localeCompare, and undefined would
// take the numeric path and order the rows nonsensically.
const BARE = { ...EMPTY, agent_id: "laptop", series: SNAP.series };
const bareRow = hostRow(BARE);
check("no host object is not an error", bareRow !== null);
check(
  "absent instance fields are strings",
  ["instanceID", "instanceType", "zone", "account"].every((k) => bareRow[k] === "")
);

// OS is read from the host, not asserted. It was the literal string "linux"
// on every row — the build target rather than an observation — which made the
// column useless for the only thing a fleet column is for: telling rows apart.
console.log("host os");
const UBUNTU = { ...EMPTY, agent_id: "web-1", series: SNAP.series, host: {
  "os.type": "linux", "os.name": "Ubuntu", "os.version": "24.04",
  "os.description": "Ubuntu 24.04.1 LTS (Linux 6.8.0-1017-aws)", "host.arch": "amd64",
}};
const AMZN = { ...EMPTY, agent_id: "web-2", series: SNAP.series, host: {
  "os.type": "linux", "os.name": "Amazon Linux", "os.version": "2023", "host.arch": "arm64",
}};
check("distribution and version", hostRow(UBUNTU).os === "Ubuntu 24.04");
check("two distributions differ", hostRow(UBUNTU).os !== hostRow(AMZN).os);
check("description carried for the tooltip", hostRow(UBUNTU).osDescription.includes("6.8.0-1017-aws"));
check("arch carried", hostRow(AMZN).arch === "arm64");

// An agent predating OS detection sends os.type only, or nothing at all. It
// must read as unknown rather than as a confident "linux" the agent never
// measured.
const TYPE_ONLY = { ...EMPTY, agent_id: "old", series: SNAP.series, host: { "os.type": "linux" } };
check("falls back to os.type", hostRow(TYPE_ONLY).os === "linux");
check("no host object means unknown, not linux", hostRow(BARE).os === "");
check("unknown os is a string", typeof hostRow(BARE).os === "string");

// The account was derived and then never rendered — the column did not exist —
// which left a fleet spanning several accounts looking like one flat list.
console.log("host account");
check("account is surfaced", hostRow(EC2).account === EC2.host["cloud.account.id"]);
check("absent account is a string", hostRow(BARE).account === "");
// The AMI was collected by the agent and dropped on the floor here.
check("ami is surfaced", hostRow(EC2).imageID === EC2.host["host.image.id"]);
check("absent ami is a string", hostRow(BARE).imageID === "");

// A malformed trace must not hang the render.
console.log("malformed input");
const CYCLE = { ...EMPTY, spans: [
  { t: T0, trace_id: "c", span_id: "a", parent_id: "b", service: "x", name: "a", dur_ms: 1 },
  { t: T0, trace_id: "c", span_id: "b", parent_id: "a", service: "x", name: "b", dur_ms: 1 },
]};
check("parent cycle terminates", deriveTraces(CYCLE)[0].spans.length === 2);

// A severity the record carries beats one guessed from its text.
//
// Lines tailed from a file have no severity, which is why the text classifier
// exists. An OTLP record - every log the backend stores - has one the writer
// set, and preferring the guess over it marks a line explicitly written as
// WARN as INFO because the word does not appear in the message.
console.log("log severity");
const REPORTED = { ...EMPTY, logs: [
  { t: T0, source: "app", message: "connection pool at 80% capacity", labels: { level: "WARN" } },
  { t: T0 + 1, source: "app", message: "upstream timeout", labels: { level: "ERROR" } },
  { t: T0 + 2, source: "app", message: "starting", labels: { level: "warning" } },
  { t: T0 + 3, source: "app", message: "ERROR retrying is fine", labels: { level: "INFO" } },
  { t: T0 + 4, source: "app", message: "plain line with no label" },
  { t: T0 + 5, source: "app", message: "ERROR something broke" },
  { t: T0 + 6, source: "app", message: "labelled with nonsense", labels: { level: "SEVERE" } },
]};
const rl = deriveLogs(REPORTED).slice().reverse(); // deriveLogs reverses
check("reported WARN is used", rl[0].lvl === "WARN", rl[0].lvl);
check("reported ERROR is used", rl[1].lvl === "ERROR", rl[1].lvl);
check("reported level is normalised", rl[2].lvl === "WARN", rl[2].lvl);
// The case the text classifier gets wrong on its own.
check("reported level beats the text guess", rl[3].lvl === "INFO", rl[3].lvl);
check("reported level is marked as reported", rl[0].lvlSource === "record", rl[0].lvlSource);
check("unlabelled line still falls back to INFO", rl[4].lvl === "INFO", rl[4].lvl);
check("unlabelled line still reads its text", rl[5].lvl === "ERROR", rl[5].lvl);
check("guessed level is stamped with the adapter version",
  rl[5].lvlSource === `client:${ADAPTER_VERSION}`, rl[5].lvlSource);
// An unrecognised label must not be trusted blindly, and must not blank the
// level either.
check("unrecognised label falls back", rl[6].lvl === "INFO", rl[6].lvl);

// Correlation, when the record carries it. A tailed file line has nothing to
// correlate on and must stay null so the UI hides the affordance.
console.log("log correlation");
const CORR = { ...EMPTY, logs: [
  { t: T0, source: "app", message: "handled", labels: { trace_id: "5b8efff798038103" } },
  { t: T0 + 1, source: "/var/log/syslog", message: "no trace here" },
]};
const cl = deriveLogs(CORR).slice().reverse();
check("trace id is surfaced", cl[0].traceId === "5b8efff798038103", cl[0].traceId);
check("absent trace id stays null", cl[1].traceId === null, String(cl[1].traceId));

console.log(failed === 0 ? "\nOK — all adapter checks passed" : `\n${failed} check(s) failed`);
process.exit(failed ? 1 : 0);
