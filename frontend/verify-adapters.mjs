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
