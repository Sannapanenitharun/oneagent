// Adapters: Agent-I's /api/snapshot -> the shapes the dashboard views render.
//
// The agent ships a deliberately dumb payload — raw series, raw log lines,
// raw spans — and every derived number is computed here. That split is on
// purpose: the agent stays a collector, and changing how a percentile or a
// health threshold is defined does not require redeploying to every host.
//
// Where the backend genuinely cannot answer something, these return empty
// rather than inventing a plausible value. A dashboard that fabricates is
// worse than one that admits a gap.

// ADAPTER_VERSION identifies the derivation semantics in this file — how a
// percentile is taken, where a health threshold sits, how a counter becomes a
// rate, how a log line's severity is decided.
//
// It is not the agent's version and cannot be: this file ships with the UI and
// the agent ships separately, so the same agent can be read by two different
// builds of these adapters. The agent's snapshot carries `adapter_contract`,
// which is the contract this file is written against. They answer different
// questions — the payload says what shape it is, this says how it was read —
// and CONTRACT below is where the two meet.
//
// Bump this when a derived number's meaning changes, so a stored or exported
// value can be traced back to the logic that produced it.
export const ADAPTER_VERSION = "1";

// The value of `adapter_contract` this file knows how to read. If a snapshot
// declares something else, the agent and the UI disagree about the payload and
// a consumer should treat derived values with suspicion.
export const CONTRACT = "1";

// Reports whether a snapshot's declared contract is one these adapters
// understand. Informational: nothing here refuses to render on a mismatch,
// because a UI that blanks itself on a version skew is less useful than one
// that shows the data and says it might be reading it wrong.
export function contractMatches(snap) {
  const declared = snap?.adapter_contract;
  // Absent means an agent older than the field — readable, just unlabelled.
  return declared == null || declared === CONTRACT;
}

// ---------------------------------------------------------------------------
// series helpers
// ---------------------------------------------------------------------------

export function pick(snap, name) {
  return (snap?.series || []).filter((s) => s.name === name);
}

// A cumulative counter only ever climbs, so plotting it raw says nothing.
// Differentiating recovers the per-second rate it was actually measuring. A
// negative delta means the counter reset (process restart, interface reset),
// so that interval is dropped rather than drawn as a huge negative spike.
export function toRate(points) {
  const out = [];
  for (let i = 1; i < points.length; i++) {
    const dt = (points[i].t - points[i - 1].t) / 1000;
    if (dt <= 0) continue;
    const dv = points[i].v - points[i - 1].v;
    if (dv < 0) continue;
    out.push({ t: points[i].t, v: dv / dt });
  }
  return out;
}

export const prepare = (s) => (s.cumulative ? toRate(s.points) : s.points);
export const latest = (pts) => (pts.length ? pts[pts.length - 1].v : 0);

const clock = (t) =>
  new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

// Recharts wants [{time, value}]; the agent speaks [{t, v}].
export const toChart = (points) =>
  points.map((p) => ({ time: clock(p.t), value: Math.round(p.v * 100) / 100 }));

// Sums series that differ only by a label we don't care about (per-device
// network or disk counters), aligned on timestamp. A host with six interfaces
// should read as one throughput line, not six that each mean nothing alone.
export function sumBy(snap, name, label) {
  const groups = {};
  for (const s of pick(snap, name)) {
    const key = s.labels?.[label] ?? "—";
    groups[key] = groups[key] || {};
    for (const p of prepare(s)) groups[key][p.t] = (groups[key][p.t] || 0) + p.v;
  }
  return Object.entries(groups).map(([key, byT]) => ({
    key,
    points: Object.keys(byT)
      .map(Number)
      .sort((a, b) => a - b)
      .map((t) => ({ t, v: byT[t] })),
  }));
}

// ---------------------------------------------------------------------------
// percentiles
// ---------------------------------------------------------------------------

// Nearest-rank, matching how the agent's own span stats compute percentiles,
// so a number derived here agrees with the same number derived server-side.
function percentile(sorted, p) {
  if (!sorted.length) return 0;
  const idx = Math.min(sorted.length - 1, Math.floor(p * sorted.length));
  return sorted[idx];
}

const isError = (s) => s.status === "2" || s.status === "ERROR";

// ---------------------------------------------------------------------------
// services (derived from spans)
// ---------------------------------------------------------------------------

// The agent does not maintain a service registry; services are whatever has
// sent spans recently. Derived rather than configured, which means the list is
// always accurate and never needs maintaining.
export function deriveServices(snap) {
  const spans = snap?.spans || [];
  if (!spans.length) return [];

  const windowSec = Math.max(1, (snap.retain_sec || 900));
  const byService = new Map();

  for (const sp of spans) {
    const id = sp.service || "unknown";
    if (!byService.has(id)) byService.set(id, { durs: [], errors: 0, count: 0 });
    const e = byService.get(id);
    e.count++;
    e.durs.push(sp.dur_ms);
    if (isError(sp)) e.errors++;
  }

  return [...byService.entries()]
    .map(([id, e]) => {
      const sorted = e.durs.slice().sort((a, b) => a - b);
      const p50 = Math.round(percentile(sorted, 0.5));
      const p99 = Math.round(percentile(sorted, 0.99));
      const err = e.count ? (e.errors / e.count) * 100 : 0;
      return {
        id,
        label: id,
        p50,
        p99,
        count: e.count,
        // Normalised over the retention window, which is what "requests per
        // second over the last N minutes" means. Kept to 3 decimals so a low
        // rate reads as 0.004 rather than rounding away to a flat 0 and
        // looking like no traffic at all.
        rps: Math.round((e.count / windowSec) * 1000) / 1000,
        err: Math.round(err * 100) / 100,
        // Thresholds live here, not in the agent: "degraded" is a product
        // judgement, and baking it into the collector would mean a redeploy
        // to change an opinion.
        status: err > 1 || p99 > 300 ? "degraded" : "healthy",
      };
    })
    .sort((a, b) => b.rps - a.rps);
}

// ---------------------------------------------------------------------------
// traces
// ---------------------------------------------------------------------------

// Rebuilds each trace's call tree from parent_id. Depth drives both the
// waterfall indent and the flame graph's vertical stacking; without the parent
// link every span would collapse to depth 0 and the two views would be
// indistinguishable from a flat list.
export function deriveTraces(snap) {
  const spans = snap?.spans || [];
  if (!spans.length) return [];

  const byTrace = new Map();
  for (const sp of spans) {
    const id = sp.trace_id || "(none)";
    if (!byTrace.has(id)) byTrace.set(id, []);
    byTrace.get(id).push(sp);
  }

  const traces = [];
  for (const [id, list] of byTrace) {
    const byId = new Map(list.map((s) => [s.span_id, s]));

    // Walk to the root, with a visited set so a malformed trace containing a
    // parent cycle cannot hang the render.
    const depthOf = (sp) => {
      let d = 0;
      let seen = new Set([sp.span_id]);
      let cur = sp;
      while (cur.parent_id && byId.has(cur.parent_id) && !seen.has(cur.parent_id)) {
        seen.add(cur.parent_id);
        cur = byId.get(cur.parent_id);
        d++;
      }
      return d;
    };

    const t0 = Math.min(...list.map((s) => s.t));
    const end = Math.max(...list.map((s) => s.t + s.dur_ms));
    // A span whose parent is missing from this window is treated as a root:
    // the store keeps a bounded number of spans, so a long trace can be
    // partially evicted, and refusing to render the remainder is worse than
    // rendering it slightly shallower.
    const roots = list.filter((s) => !s.parent_id || !byId.has(s.parent_id));
    const root = roots.length
      ? roots.reduce((a, b) => (a.t <= b.t ? a : b))
      : list.reduce((a, b) => (a.t <= b.t ? a : b));

    traces.push({
      id,
      root: root.service || "unknown",
      op: root.name || "—",
      duration: Math.max(1, Math.round(end - t0)),
      status: list.some(isError) ? "error" : "ok",
      startedAt: t0,
      spans: list
        .slice()
        .sort((a, b) => a.t - b.t)
        .map((s) => ({
          svc: s.service || "unknown",
          op: s.name || "—",
          start: Math.max(0, Math.round(s.t - t0)),
          dur: Math.round(s.dur_ms),
          depth: depthOf(s),
          error: isError(s),
        })),
    });
  }
  return traces.sort((a, b) => b.startedAt - a.startedAt);
}

// Caller -> callee edges, derived the way OpenTelemetry's service graph
// connector derives them, because the naive reading of parent links gets two
// things wrong that matter.
//
// An edge between two services is a CLIENT span whose child is a SERVER span
// in another service. Pairing on parent-child alone cannot tell that from an
// ordinary nested call, and — worse — it silently drops every dependency that
// is not itself instrumented. A service's database, its queue and the
// third-party API it calls produce a CLIENT span with no matching SERVER span
// anywhere in the trace, so under the old derivation they did not exist. Those
// are usually the dependencies a topology is being read to find.
//
// So this pairs on kind, and where a client span has no server span to pair
// with it reads the peer's identity out of the span's own attributes and
// creates a virtual node — the purple "inferred dependency" every commercial
// map shows.
//
// Edges carry their traffic rather than only their existence. An unweighted
// graph draws a dependency serving one request an hour identically to one
// serving a thousand a second, and cannot show which edge is the failing one.

// PEER_NAMERS resolve what an uninstrumented peer should be called, in
// priority order. peer.service is the explicit answer when an SDK sets it;
// everything after it is inference from what the span was doing.
const PEER_NAMERS = [
  (p) => p["peer.service"],
  (p) => p["db.namespace"] || p["db.name"],
  (p) => p["messaging.destination.name"] || p["messaging.destination"],
  (p) => p["rpc.service"],
  (p) => p["server.address"] || p["net.peer.name"],
  (p) => p["db.system.name"] || p["db.system"],
  (p) => p["messaging.system"],
];

// peerName resolves the display name of an uninstrumented dependency, or ""
// when the span said nothing about who it was talking to — in which case no
// edge is invented, because a node called "unknown" is worse than an absent
// one.
export function peerName(peer) {
  if (!peer) return "";
  for (const namer of PEER_NAMERS) {
    const v = namer(peer);
    if (v) return String(v);
  }
  return "";
}

// peerType classifies a dependency so the map can draw a datastore differently
// from a queue differently from an HTTP service. Mirrors the connection_type
// dimension the service graph connector puts on its metrics.
export function peerType(peer) {
  if (!peer) return "service";
  if (peer["db.system"] || peer["db.system.name"] || peer["db.name"] || peer["db.namespace"]) return "database";
  if (peer["messaging.system"] || peer["messaging.destination"] || peer["messaging.destination.name"]) return "messaging";
  return "service";
}

const isServerSide = (k) => k === "server" || k === "consumer";
const isClientSide = (k) => k === "client" || k === "producer";

// deriveEdges returns one aggregated edge per caller/callee pair.
//
// Each carries calls, errors and a latency distribution, so the map can encode
// volume as thickness and failure as colour instead of drawing every
// dependency the same weight.
export function deriveEdges(snap) {
  const spans = snap?.spans || [];
  if (!spans.length) return [];

  const byId = new Map(spans.map((s) => [s.span_id, s]));
  const childrenOf = new Map();
  for (const s of spans) {
    if (!s.parent_id) continue;
    if (!childrenOf.has(s.parent_id)) childrenOf.set(s.parent_id, []);
    childrenOf.get(s.parent_id).push(s);
  }

  // Whether the agent reporting these spans sends span kinds at all. Older
  // agents do not, and on those the only derivation available is the parent
  // link — worse, but far better than an empty map.
  const hasKinds = spans.some((s) => s.kind);
  const known = new Set(spans.map((s) => s.service).filter(Boolean));

  const acc = new Map();
  const add = (from, to, { error, durMs, type, virtual }) => {
    if (!from || !to || from === to) return;
    const key = `${from}\u0000${to}`;
    let e = acc.get(key);
    if (!e) {
      e = { from, to, calls: 0, errors: 0, durs: [], type: type || "service", virtual: !!virtual };
      acc.set(key, e);
    }
    e.calls++;
    if (error) e.errors++;
    if (Number.isFinite(durMs)) e.durs.push(durMs);
    // An edge is only virtual while every call on it was to an unpaired peer.
    // One real server span proves the far side is instrumented after all.
    if (!virtual) e.virtual = false;
    if (type === "database" || type === "messaging") e.type = type;
  };

  for (const sp of spans) {
    const service = sp.service || "";
    if (!service) continue;

    // --- paired: a server span whose parent is a client span elsewhere ---
    const parent = sp.parent_id ? byId.get(sp.parent_id) : null;
    const pairedAsServer = parent && (!hasKinds || (isServerSide(sp.kind) && isClientSide(parent.kind)));
    if (pairedAsServer) {
      add(parent.service || "", service, {
        // Either side failing makes the call a failed call: a caller that
        // errored on a response the callee considered fine still had the
        // request fail, and vice versa.
        error: isError(sp) || isError(parent),
        // The caller's duration, which is what the caller experienced —
        // it includes the network and any queueing the callee never saw.
        durMs: Number.isFinite(parent.dur_ms) ? parent.dur_ms : sp.dur_ms,
        type: sp.kind === "consumer" ? "messaging" : "service",
        virtual: false,
      });
      continue;
    }

    // --- unpaired: an outbound span with nothing on the other end ---
    if (!hasKinds || !isClientSide(sp.kind)) continue;
    const children = childrenOf.get(sp.span_id) || [];
    if (children.some((c) => (c.service || "") !== service)) continue; // paired above

    const name = peerName(sp.peer);
    if (!name || name === service) continue;
    // A peer that names an instrumented service is not virtual — its spans are
    // simply outside this window, which the rolling buffer makes routine. This
    // is what stops a real service flickering into a separate inferred node
    // whenever its own spans age out first.
    add(service, name, {
      error: isError(sp),
      durMs: sp.dur_ms,
      type: peerType(sp.peer),
      virtual: !known.has(name),
    });
  }

  return [...acc.values()]
    .map((e) => {
      const sorted = e.durs.slice().sort((a, b) => a - b);
      return {
        from: e.from,
        to: e.to,
        calls: e.calls,
        errors: e.errors,
        errPct: e.calls ? Math.round((e.errors / e.calls) * 10000) / 100 : 0,
        p50: Math.round(percentile(sorted, 0.5)),
        p99: Math.round(percentile(sorted, 0.99)),
        type: e.type,
        virtual: e.virtual,
      };
    })
    .sort((a, b) => b.calls - a.calls);
}

// deriveTopologyNodes returns every node the map should draw: the instrumented
// services, plus a node for each inferred dependency an edge points at.
//
// Virtual nodes are built here rather than in deriveServices because they are
// not services — nothing reported them, they have no latency of their own and
// no health to speak of. Putting them in the services list would have them
// counted in "healthy / seen" and listed in the services table, both of which
// would be claims the data does not support.
export function deriveTopologyNodes(services, edges) {
  const nodes = services.map((s) => ({ ...s, virtual: false }));
  const have = new Set(nodes.map((n) => n.id));

  for (const e of edges) {
    if (!e.virtual || have.has(e.to)) continue;
    have.add(e.to);
    const inbound = edges.filter((x) => x.to === e.to);
    const calls = inbound.reduce((a, x) => a + x.calls, 0);
    const errors = inbound.reduce((a, x) => a + x.errors, 0);
    nodes.push({
      id: e.to,
      label: e.to,
      virtual: true,
      type: e.type,
      calls,
      // An inferred node's health is entirely what its callers saw, since it
      // reported nothing itself. Said plainly rather than shown as unknown:
      // a database failing every call is worth drawing as failing.
      err: calls ? Math.round((errors / calls) * 10000) / 100 : 0,
      status: calls && errors / calls > 0.01 ? "degraded" : "healthy",
      p50: Math.round(Math.min(...inbound.map((x) => x.p50))),
      p99: Math.round(Math.max(...inbound.map((x) => x.p99))),
      rps: 0,
    });
  }
  return nodes;
}

// Lays out a derived graph in dependency order: roots on the left, each node
// one column right of its deepest caller. Positions cannot be hardcoded when
// the topology itself is discovered at runtime.
export function layoutTopology(services, edges, width = 460, height = 190) {
  if (!services.length) return {};
  const incoming = new Map(services.map((s) => [s.id, 0]));
  for (const { to } of edges) incoming.set(to, (incoming.get(to) || 0) + 1);

  const col = new Map();
  const roots = services.filter((s) => !incoming.get(s.id));
  const queue = (roots.length ? roots : [services[0]]).map((s) => s.id);
  queue.forEach((id) => col.set(id, 0));

  for (let guard = 0; queue.length && guard < 500; guard++) {
    const cur = queue.shift();
    for (const { from, to } of edges) {
      if (from !== cur) continue;
      const next = (col.get(cur) || 0) + 1;
      if ((col.get(to) ?? -1) < next) {
        col.set(to, next);
        queue.push(to);
      }
    }
  }
  for (const s of services) if (!col.has(s.id)) col.set(s.id, 0);

  const maxCol = Math.max(...col.values(), 0);
  const rows = new Map();
  const pos = {};
  for (const s of services) {
    const c = col.get(s.id);
    const r = rows.get(c) || 0;
    rows.set(c, r + 1);
    pos[s.id] = { col: c, row: r };
  }
  const out = {};
  for (const s of services) {
    const { col: c, row: r } = pos[s.id];
    const count = rows.get(c);
    out[s.id] = {
      x: 50 + (maxCol ? (c / maxCol) * (width - 110) : 0),
      y: (height / (count + 1)) * (r + 1),
    };
  }
  return out;
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

// The agent tails files and forwards lines verbatim — it does not parse
// severity, and inventing a parser server-side would impose one log format on
// every deployment. Classifying here keeps that policy in the UI where it can
// be changed without touching a single host.
const LEVEL_RE = /\b(FATAL|CRITICAL|ERROR|ERR|WARN(?:ING)?|INFO|DEBUG|TRACE)\b/i;

// Pulls a JSON object out of a log line, if there is one.
//
// Structured loggers write either a bare JSON object per line (MongoDB, most
// Go and Java loggers) or a plain-text prefix followed by one. Both are worth
// parsing: a 900-character object rendered as a single string is unreadable,
// and the fields inside it are the reason the app logged JSON in the first
// place.
//
// Parsed in the browser, not the agent — same rule as severity. The agent
// forwards lines verbatim and imposes no log format, because deciding that
// every deployment logs JSON is not a collector's call to make.
export function parseLogBody(message) {
  const start = message.indexOf("{");
  if (start === -1) return null;
  // Only an object is worth a field tree; a bare array or scalar is not.
  const candidate = message.slice(start);
  if (!candidate.endsWith("}")) return null;
  try {
    const value = JSON.parse(candidate);
    if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
    return { prefix: message.slice(0, start).trim(), value };
  } catch {
    // Not JSON, or JSON with trailing text after it. Either way the raw line
    // is what we have, and guessing at partial structure would invent fields.
    return null;
  }
}

// Flattens a parsed body into dotted paths, the form a field list wants.
// Depth-limited: a deeply nested object is a sign of an embedded document, and
// past a few levels a flat list stops being easier to read than the raw JSON,
// which the Raw tab already shows in full.
export function flattenFields(value, prefix = "", depth = 0, out = []) {
  if (depth > 6) return out;
  for (const [k, v] of Object.entries(value)) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (v !== null && typeof v === "object" && !Array.isArray(v)) {
      flattenFields(v, path, depth + 1, out);
    } else {
      out.push({ path, value: Array.isArray(v) ? JSON.stringify(v) : v });
    }
  }
  return out;
}

// normalizeLevel maps the many spellings of a severity onto the five the UI
// filters by. Returns "" for anything unrecognised, so a caller can tell an
// absent level from one it could not read.
function normalizeLevel(raw) {
  const tok = String(raw || "").trim().toUpperCase();
  if (!tok) return "";
  if (tok === "FATAL" || tok === "CRITICAL" || tok === "CRIT" || tok === "ERR") return "ERROR";
  if (tok === "WARNING") return "WARN";
  if (["ERROR", "WARN", "INFO", "DEBUG", "TRACE"].includes(tok)) return tok;
  return "";
}

export function deriveLogs(snap) {
  return (snap?.logs || [])
    .slice()
    .reverse()
    .map((l) => {
      // A severity the record actually carries beats one guessed from its
      // text. Lines tailed from a file have none — which is why the regex
      // below exists — but an OTLP record has a severity the application set,
      // and preferring the guess over it would classify a line the writer
      // explicitly marked WARN as INFO because the word does not appear in the
      // message.
      const reported = normalizeLevel(l.labels?.level);
      let lvl = reported || "INFO";
      if (!reported) {
        const m = LEVEL_RE.exec(l.message || "");
        if (m) lvl = normalizeLevel(m[1]) || "INFO";
      }
      return {
        t: new Date(l.t).toLocaleTimeString([], { hour12: false }),
        // Kept alongside the display string so the detail view can show a full
        // timestamp without re-deriving it from a formatted one.
        tms: l.t,
        lvl,
        // Where lvl came from. The agent forwards log lines verbatim and parses
        // no levels, so this value was guessed from the line's text by the
        // regex above — not reported by the application that wrote it.
        //
        // Recording that matters because the guess is the part most likely to
        // change: a line reading "ERROR handling retry" is INFO or ERROR
        // depending on where the pattern anchors. Stamping the adapter version
        // means a severity that later looks wrong can be traced to the
        // classifier that produced it, instead of being assumed authoritative.
        lvlSource: reported ? "record" : `client:${ADAPTER_VERSION}`,
        // Source is the file the line was tailed from; its basename is the
        // most useful short identifier available.
        svc: (l.source || "").split(/[\\/]/).pop() || "log",
        src: l.source || "",
        labels: l.labels || null,
        msg: l.message || "",
        structured: parseLogBody(l.message || ""),
        // Correlation, when the record carries it. An OTLP log record has a
        // trace id field and the backend passes it through; a line tailed from
        // a file has nothing to correlate on and stays null, so the UI hides
        // the jump-to-trace affordance rather than offering one that goes
        // nowhere.
        traceId: l.labels?.trace_id || null,
      };
    });
}

// ---------------------------------------------------------------------------
// infrastructure (this agent's own host)
// ---------------------------------------------------------------------------

export function deriveInfra(snap) {
  if (!snap?.series?.length) return [];

  // CPU busy % = 1 - idle share of total cpu-seconds per second.
  const cpuSeries = pick(snap, "system.cpu.time").map((s) => ({
    state: s.labels?.state,
    points: prepare(s),
  }));
  let cpu = NaN;
  if (cpuSeries.length) {
    const total = cpuSeries.reduce((a, s) => a + latest(s.points), 0);
    const idle = latest(cpuSeries.find((s) => s.state === "idle")?.points || []);
    if (total > 0) cpu = Math.round((1 - idle / total) * 100);
  }
  if (Number.isNaN(cpu)) {
    cpu = Math.round(latest(pick(snap, "host.cpu.used_pct")[0]?.points || []));
  }

  const mem = pick(snap, "system.memory.usage");
  let memPct = NaN;
  if (mem.length) {
    const total = mem.reduce((a, s) => a + latest(s.points), 0);
    const used = latest(mem.find((s) => s.labels?.state === "used")?.points || []);
    if (total > 0) memPct = Math.round((used / total) * 100);
  }
  if (Number.isNaN(memPct)) {
    memPct = Math.round(latest(pick(snap, "host.memory.used_pct")[0]?.points || []));
  }

  // Worst mountpoint, since a host is in trouble when any filesystem fills,
  // not when their average does.
  const mounts = {};
  for (const s of pick(snap, "system.filesystem.usage")) {
    const mp = s.labels?.mountpoint || "?";
    mounts[mp] = mounts[mp] || {};
    mounts[mp][s.labels?.state || "?"] = latest(s.points);
  }
  let disk = 0;
  const perMount = [];
  for (const [mp, v] of Object.entries(mounts)) {
    const tot = (v.used || 0) + (v.free || 0);
    if (tot <= 0) continue;
    const pct = Math.round((v.used / tot) * 100);
    perMount.push({ mount: mp, pct });
    if (pct > disk) disk = pct;
  }

  const load1 = latest(pick(snap, "system.cpu.load_average.1m")[0]?.points || []);

  return [
    {
      host: snap.agent_id || "unknown",
      role: `agent-i ${snap.version || ""}`.trim(),
      cpu: Number.isFinite(cpu) ? cpu : 0,
      mem: Number.isFinite(memPct) ? memPct : 0,
      disk,
      load1: Math.round(load1 * 100) / 100,
      mounts: perMount.sort((a, b) => b.pct - a.pct),
      status: cpu > 85 || memPct > 90 || disk > 90 ? "degraded" : "healthy",
    },
  ];
}

// hostRow reduces one agent's snapshot to the columns a host list shows.
//
// The column set follows the one SigNoz's host monitoring settles on —
// hostname, status, CPU, memory, IOWait, disk, load average — because those
// are the seven facts that decide whether a host needs attention, and they
// happen to be derivable from metrics this agent already emits. That is not a
// coincidence: both read the OpenTelemetry hostmetrics conventions, so
// system.cpu.time / system.memory.usage / system.filesystem.usage /
// system.cpu.load_average.15m mean the same thing on both sides.
//
// IOWait earns its column despite looking like a CPU detail. A host pinned on
// iowait is not busy, it is BLOCKED — the CPU is idle waiting for a disk that
// cannot keep up. Read from CPU usage alone that host looks fine.
export function hostRow(snap) {
  const base = deriveInfra(snap)[0];
  if (!base) return null;

  // IOWait as a share of total CPU time, from the same rates the CPU column
  // uses, so the two columns cannot disagree.
  const cpuStates = pick(snap, "system.cpu.time").map((s) => ({
    state: s.labels?.state,
    points: prepare(s),
  }));
  let iowait = NaN;
  if (cpuStates.length) {
    const total = cpuStates.reduce((a, s) => a + latest(s.points), 0);
    const wait = latest(cpuStates.find((s) => s.state === "iowait")?.points || []);
    if (total > 0) iowait = (wait / total) * 100;
  }

  // SigNoz calls a host ACTIVE when any metric arrived in the last 10 minutes.
  // Worth having as its own column rather than inferring from the numbers: a
  // host that stopped reporting keeps its last values, so stale data reads as
  // healthy right up until someone notices the timestamps.
  const newest = (snap.series || []).reduce((max, s) => {
    const p = s.points;
    return p && p.length ? Math.max(max, p[p.length - 1].t) : max;
  }, 0);
  const ageSec = newest && snap.now ? (snap.now - newest) / 1000 : Infinity;

  // What the machine is, as the agent discovered it — distinct from agent_id
  // above, which is only what it calls itself. Absent off a cloud host, so
  // every field defaults to "" rather than undefined: the fleet table sorts
  // these as strings, and a missing key would fall through to the numeric
  // comparison and sort nonsensically against the rows that do have one.
  const host = snap.host || {};

  return {
    host: base.host,
    instanceID: host["host.id"] || "",
    instanceType: host["host.type"] || "",
    // Zone is the more specific of the two and the one that matters when a
    // single AZ is having a bad day; region is the fallback on the rare
    // instance that reports one without the other.
    zone: host["cloud.availability_zone"] || host["cloud.region"] || "",
    account: host["cloud.account.id"] || "",
    // The AMI. Collected by the agent since EC2 detection landed and never
    // surfaced anywhere until now — the same oversight as the account above.
    // It is what answers "are these two hosts even running the same image",
    // which is the first question when one of a pair misbehaves.
    imageID: host["host.image.id"] || "",
    version: snap.version || "",
    // Read from the host, not asserted. This was the literal string "linux"
    // for every row, which is the build target rather than an observation and
    // told you nothing you could act on: a fleet is worth a column here only
    // if the column can differ between rows.
    //
    // os.name/os.version are the distribution ("Ubuntu 24.04"); os.type is the
    // kernel family and is the floor for a host whose /etc/os-release could
    // not be read. Empty rather than guessed when the agent predates the
    // detection, so an old agent reads as unknown instead of as Linux.
    os:
      [host["os.name"], host["os.version"]].filter(Boolean).join(" ") ||
      host["os.description"] ||
      host["os.type"] ||
      "",
    osDescription: host["os.description"] || "",
    arch: host["host.arch"] || "",
    active: ageSec <= 600,
    ageSec,
    cpu: base.cpu,
    mem: base.mem,
    iowait: Number.isFinite(iowait) ? Math.round(iowait * 100) / 100 : 0,
    disk: base.disk,
    load15: Math.round((latest(pick(snap, "system.cpu.load_average.15m")[0]?.points || []) || 0) * 100) / 100,
    mounts: base.mounts,
    seriesCount: snap.series?.length || 0,
    logsCount: snap.logs?.length || 0,
    spansCount: snap.spans?.length || 0,
    dropped: snap.series_dropped || 0,
    pending: snap.reload_pending_restart || [],
  };
}

// ---------------------------------------------------------------------------
// overview charts
// ---------------------------------------------------------------------------

// Buckets spans into per-minute counts. Derived from the spans actually
// retained, so it reflects what was sampled and forwarded, not the exact
// totals — the agent's trace stats are the exact source when enabled.
export function deriveTraffic(snap, bucketMs = 60000) {
  const spans = snap?.spans || [];
  if (!spans.length) return { rps: [], latency: [], errors: [] };

  const buckets = new Map();
  for (const sp of spans) {
    const b = Math.floor(sp.t / bucketMs) * bucketMs;
    if (!buckets.has(b)) buckets.set(b, { count: 0, errors: 0, durs: [] });
    const e = buckets.get(b);
    e.count++;
    e.durs.push(sp.dur_ms);
    if (isError(sp)) e.errors++;
  }

  const keys = [...buckets.keys()].sort((a, b) => a - b);
  return {
    rps: keys.map((k) => ({
      time: clock(k),
      value: Math.round((buckets.get(k).count / (bucketMs / 1000)) * 100) / 100,
    })),
    latency: keys.map((k) => ({
      time: clock(k),
      value: Math.round(percentile(buckets.get(k).durs.slice().sort((a, b) => a - b), 0.99)),
    })),
    errors: keys.map((k) => ({ time: clock(k), value: buckets.get(k).errors })),
  };
}

// Every series the agent is producing, for the raw explorer.
export function deriveAllSeries(snap) {
  return (snap?.series || []).map((s) => ({
    name: s.name,
    cumulative: s.cumulative,
    labels: s.labels
      ? Object.keys(s.labels)
          .sort()
          .map((k) => `${k}=${s.labels[k]}`)
          .join(" ")
      : "",
    latest: latest(prepare(s)),
    points: s.points.length,
  }));
}

// Formats a rate without lying about precision: a busy service reads as a
// round number, a trickle still reads as a nonzero value rather than "0".
export function fmtRps(v) {
  if (v >= 100) return Math.round(v).toString();
  if (v >= 1) return v.toFixed(1);
  if (v > 0) return v.toFixed(3);
  return "0";
}

// ---------------------------------------------------------------------------
// host metric panels
// ---------------------------------------------------------------------------

// Merges several series onto one time axis. Recharts wants a row per
// timestamp with a column per series; the agent speaks one array per series.
//
// A series missing at a timestamp gets null rather than 0 or a carried-forward
// value. Nulls draw as a gap, which is the truth — "we have no sample here" is
// not the same statement as "the value was zero", and on an error or drop chart
// those two read as opposite conclusions.
export function alignSeries(list) {
  const stamps = new Set();
  for (const s of list) for (const p of s.points) stamps.add(p.t);
  const sorted = [...stamps].sort((a, b) => a - b);

  const byKey = list.map((s) => {
    const m = new Map();
    for (const p of s.points) m.set(p.t, p.v);
    return { key: s.key, m };
  });

  const rows = sorted.map((t) => {
    const row = { t, time: clock(t) };
    for (const { key, m } of byKey) row[key] = m.has(t) ? m.get(t) : null;
    return row;
  });
  return { rows, keys: list.map((s) => s.key) };
}

// Categorical palettes are a fixed set of hues, and a chart must never invent
// a new one for series N+1 — two generated hues are indistinguishable long
// before the eye runs out of patience. Past the limit the smallest series are
// summed into a single "other" entry, ranked by peak rather than by last value
// so a device that spiked and settled is not mistaken for an idle one.
export const MAX_SERIES_PER_PANEL = 6;

export function foldSmallest(list, max = MAX_SERIES_PER_PANEL) {
  if (list.length <= max) return list;
  const peak = (s) => s.points.reduce((a, p) => Math.max(a, Math.abs(p.v)), 0);
  const ranked = [...list].sort((a, b) => peak(b) - peak(a));
  const kept = ranked.slice(0, max - 1);
  const rest = ranked.slice(max - 1);

  const summed = new Map();
  for (const s of rest) {
    for (const p of s.points) summed.set(p.t, (summed.get(p.t) || 0) + p.v);
  }
  const points = [...summed.keys()]
    .sort((a, b) => a - b)
    .map((t) => ({ t, v: summed.get(t) }));

  return [...kept, { key: `other (${rest.length})`, points }];
}

// Groups a metric into one series per label combination.
function group(snap, name, keyFn) {
  return pick(snap, name)
    .map((s) => ({ key: keyFn(s.labels || {}), points: prepare(s) }))
    .filter((s) => s.points.length > 0)
    .sort((a, b) => a.key.localeCompare(b.key));
}

// Each state's share of total CPU time. system.cpu.time is a cumulative
// per-state counter, so the rates across states sum to the number of cores;
// dividing by that total is what turns it into the percentage a reader
// expects, independent of how many cores the host has.
function cpuPercentByState(snap) {
  const states = group(snap, "system.cpu.time", (l) => l.state || "?");
  if (!states.length) return [];

  const totals = new Map();
  for (const s of states) {
    for (const p of s.points) totals.set(p.t, (totals.get(p.t) || 0) + p.v);
  }
  return states.map((s) => ({
    key: s.key,
    points: s.points
      .filter((p) => (totals.get(p.t) || 0) > 0)
      .map((p) => ({ t: p.t, v: (p.v / totals.get(p.t)) * 100 })),
  }));
}

// Used share per mountpoint. Reported as a percentage because the question a
// disk chart answers is "how close to full", which a byte count cannot answer
// without also knowing capacity.
function filesystemPercent(snap) {
  const byMount = {};
  for (const s of pick(snap, "system.filesystem.usage")) {
    const mp = s.labels?.mountpoint || "?";
    const state = s.labels?.state || "?";
    byMount[mp] = byMount[mp] || {};
    byMount[mp][state] = s.points;
  }
  return Object.entries(byMount)
    .map(([mount, states]) => {
      const used = states.used || [];
      const free = new Map((states.free || []).map((p) => [p.t, p.v]));
      return {
        key: mount,
        points: used
          .filter((p) => free.has(p.t) && p.v + free.get(p.t) > 0)
          .map((p) => ({ t: p.t, v: (p.v / (p.v + free.get(p.t))) * 100 })),
      };
    })
    .filter((s) => s.points.length)
    .sort((a, b) => a.key.localeCompare(b.key));
}

const pair = (a, b) => (l) => `${l[a] || "?"}::${l[b] || "?"}`;

// The panel set, in the order a host is actually read: what it is doing (cpu,
// memory, load), then what it is talking to (network), then what it is storing
// (disk). Every panel names the metric it needs, so an empty one explains
// itself instead of rendering a blank box.
export function hostMetricPanels(snap) {
  const defs = [
    { id: "cpu", title: "CPU Usage", unit: "%", domain: [0, 100], needs: "system.cpu.time",
      series: cpuPercentByState(snap) },
    { id: "mem", title: "Memory Usage", unit: "bytes", needs: "system.memory.usage",
      series: group(snap, "system.memory.usage", (l) => l.state || "?") },
    { id: "load", title: "System Load Average", unit: "", needs: "system.cpu.load_average.*",
      series: ["1m", "5m", "15m"]
        .map((w) => ({ key: w, points: prepare(pick(snap, `system.cpu.load_average.${w}`)[0] || { points: [] }) }))
        .filter((s) => s.points.length) },
    { id: "net.io", title: "Network usage (bytes/s)", unit: "bytes/s", needs: "system.network.io",
      series: group(snap, "system.network.io", pair("device", "direction")) },
    { id: "net.packets", title: "Network usage (packets/s)", unit: "/s", needs: "system.network.packets",
      series: group(snap, "system.network.packets", pair("device", "direction")) },
    { id: "net.errors", title: "Network errors", unit: "/s", needs: "system.network.errors",
      series: group(snap, "system.network.errors", pair("device", "direction")) },
    { id: "net.drops", title: "Network drops", unit: "/s", needs: "system.network.dropped",
      series: group(snap, "system.network.dropped", pair("device", "direction")) },
    { id: "net.conn", title: "Network connections", unit: "", needs: "system.network.connections",
      series: group(snap, "system.network.connections", pair("protocol", "state")) },
    { id: "disk.io", title: "Disk I/O (bytes/s)", unit: "bytes/s", needs: "system.disk.io",
      series: group(snap, "system.disk.io", pair("device", "direction")) },
    { id: "disk.ops", title: "Disk operations/s", unit: "/s", needs: "system.disk.operations",
      series: group(snap, "system.disk.operations", pair("device", "direction")) },
    { id: "disk.queue", title: "Queue size", unit: "", needs: "system.disk.pending_operations",
      series: group(snap, "system.disk.pending_operations", (l) => l.device || "?") },
    { id: "disk.time", title: "Disk operation time/s", unit: "s/s", needs: "system.disk.operation_time",
      series: group(snap, "system.disk.operation_time", pair("device", "direction")) },
    { id: "fs", title: "Disk usage (%) by mountpoint", unit: "%", domain: [0, 100], needs: "system.filesystem.usage",
      series: filesystemPercent(snap) },
  ];

  return defs.map((d) => {
    const folded = foldSmallest(d.series);
    // One point cannot be drawn as a line. Reporting the count lets the panel
    // say "waiting for a second sample" rather than looking broken on a host
    // whose agent started ten seconds ago.
    const drawable = folded.filter((s) => s.points.length > 1);
    return { ...d, ...alignSeries(drawable), series: drawable, points: drawable[0]?.points.length || 0 };
  });
}

// Byte counts span nine orders of magnitude on the same chart, so an axis of
// raw numbers is unreadable. Binary units because that is what the kernel
// reports and what df agrees with.
export function fmtBytes(v, perSec = false) {
  const suffix = perSec ? "/s" : "";
  const abs = Math.abs(v);
  if (abs >= 1024 ** 3) return `${(v / 1024 ** 3).toFixed(1)} GiB${suffix}`;
  if (abs >= 1024 ** 2) return `${(v / 1024 ** 2).toFixed(1)} MiB${suffix}`;
  if (abs >= 1024) return `${(v / 1024).toFixed(1)} KiB${suffix}`;
  return `${Math.round(v)} B${suffix}`;
}

export function fmtMetric(v, unit) {
  if (v == null || Number.isNaN(v)) return "—";
  if (unit === "bytes") return fmtBytes(v);
  if (unit === "bytes/s") return fmtBytes(v, true);
  if (unit === "%") return `${v.toFixed(1)}%`;
  if (unit === "s/s") return `${(v * 1000).toFixed(1)} ms/s`;
  const abs = Math.abs(v);
  if (abs >= 1000) return Math.round(v).toLocaleString();
  if (abs >= 10) return v.toFixed(1);
  if (abs > 0) return v.toFixed(2);
  return "0";
}

export function globalStats(snap) {
  const services = deriveServices(snap);
  const totalRps = services.reduce((a, s) => a + s.rps, 0);
  const p99 = services.length ? Math.max(...services.map((s) => s.p99)) : NaN;
  const counts = snap?.counts || {};
  const uptimeSec = snap ? Math.max(1, (snap.now - snap.started_at) / 1000) : 1;
  const envelopes = Object.values(counts).reduce((a, b) => a + b, 0);
  return {
    services,
    totalRps: Math.round(totalRps * 1000) / 1000,
    p99,
    envelopes,
    envelopesPerSec: Math.round((envelopes / uptimeSec) * 10) / 10,
    counts,
    seriesDropped: snap?.series_dropped || 0,
  };
}
