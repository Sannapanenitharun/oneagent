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

console.log("edges");
const edges = deriveEdges(SNAP);
const has = (f, t) => edges.some(([a, b]) => a === f && b === t);
check("api-gateway -> checkout", has("api-gateway", "checkout"));
check("checkout -> payments", has("checkout", "payments"));
check("self-calls are not dependencies", !has("checkout", "checkout"));

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

// A malformed trace must not hang the render.
console.log("malformed input");
const CYCLE = { ...EMPTY, spans: [
  { t: T0, trace_id: "c", span_id: "a", parent_id: "b", service: "x", name: "a", dur_ms: 1 },
  { t: T0, trace_id: "c", span_id: "b", parent_id: "a", service: "x", name: "b", dur_ms: 1 },
]};
check("parent cycle terminates", deriveTraces(CYCLE)[0].spans.length === 2);

console.log(failed === 0 ? "\nOK — all adapter checks passed" : `\n${failed} check(s) failed`);
process.exit(failed ? 1 : 0);
