// Reading the fleet from the backend rather than from the agents themselves.
//
// The difference is not an implementation detail, it is the whole reason the
// backend was built. Reading agents directly means the browser needs a route
// to every machine: a tunnel per host, kept alive, and a host that is merely
// unreachable from this laptop looks identical to a host that is down. Reading
// the backend means one address, and a host appears because it reported —
// which it can do from another account, another region, or behind a NAT this
// machine will never see through.
//
// So the two sources answer different questions and both are kept. The fleet
// table asks "what exists and what is it doing", which only the backend can
// answer for a machine you cannot reach. A single host's detail view asks for
// raw series, logs and spans at full resolution, which the agent still serves
// directly. See api.js for that path.

import { useEffect, useState } from "react";

// Named for error messages, the same reason api.js keeps its own copy: a
// failure that says "the backend" leaves you guessing which address, and the
// address is usually the answer.
const TARGET = typeof __BACKEND_TARGET__ === "string" ? __BACKEND_TARGET__ : "the backend";

export const BACKEND_TARGET = TARGET;

// /b is the dev server's route to the backend; see vite.config.js. It exists
// so the backend's /api cannot collide with the agent's /api, which is a
// different API on a different process that happens to share a prefix.
const PREFIX = "/b";

export class BackendError extends Error {
  constructor(message, detail) {
    super(message);
    this.name = "BackendError";
    this.detail = detail;
  }
}

async function getJSON(path, signal) {
  let res;
  try {
    res = await fetch(`${PREFIX}${path}`, { cache: "no-store", signal });
  } catch (err) {
    if (err.name === "AbortError") throw err;
    throw new BackendError(
      "backend not reachable",
      `Could not connect to ${TARGET}. Start it with \`cd deploy && docker compose up -d\`, ` +
        `or set AGENT_I_BACKEND_URL if it is somewhere else.`
    );
  }
  if (!res.ok) {
    throw new BackendError(
      `backend returned HTTP ${res.status}`,
      `${TARGET} answered ${res.status} for ${path}.`
    );
  }
  // A dev-server misroute answers 200 with the SPA's index.html, which fails
  // as JSON in a way that reads like the backend returned garbage. Naming the
  // content type is what separates "not running" from "not proxied".
  try {
    return await res.json();
  } catch {
    throw new BackendError(
      "unexpected response",
      `${TARGET} answered with ${res.headers.get("content-type") || "an unknown content type"} ` +
        `instead of JSON, which usually means ${PREFIX} is not proxied to a backend.`
    );
  }
}

// A host is ACTIVE if it reported inside this window. Matches the agent-side
// definition in adapters.js so the same host does not change status depending
// on which source the table was reading.
const ACTIVE_WINDOW_SEC = 600;

// backendHostRow maps one /api/hosts entry onto the row shape the fleet table
// renders, which adapters.hostRow produces from an agent snapshot.
//
// Deliberately the same shape rather than a parallel one: the table sorts,
// filters and formats these fields, and a second shape would mean every one of
// those behaviours existing twice and drifting apart.
export function backendHostRow(h, nowMs) {
  const a = h.attributes || {};
  const seen = Date.parse(h.last_seen);
  const ageSec = Number.isFinite(seen) ? (nowMs - seen) / 1000 : Infinity;

  // NaN, not 0, for a metric the host has not reported. The table renders NaN
  // as an empty cell and sorts it last; 0 would be a claim the host is idle,
  // which is a different statement and a wrong one. The backend is careful to
  // distinguish these — cpu_pct is null rather than 0 when absent — and that
  // distinction is only worth anything if it survives to here.
  const num = (v) => (typeof v === "number" ? v : NaN);

  return {
    host: h.agent_id || h.host_id,
    instanceID: a["host.id"] || h.host_id || "",
    instanceType: a["host.type"] || "",
    zone: a["cloud.availability_zone"] || a["cloud.region"] || "",
    account: a["cloud.account.id"] || "",
    imageID: a["host.image.id"] || "",
    version: a["service.version"] || "",
    // Distro and version first, then the full description, and only then the
    // kernel family. os.type alone is "linux" for every row — the same
    // uninformative constant this column was built to replace — so it is the
    // last resort rather than the first fallback.
    os:
      [a["os.name"], a["os.version"]].filter(Boolean).join(" ") ||
      a["os.description"] ||
      a["os.type"] ||
      "",
    osDescription: a["os.description"] || "",
    arch: a["host.arch"] || "",
    active: ageSec <= ACTIVE_WINDOW_SEC,
    ageSec,
    cpu: num(h.cpu_pct),
    mem: num(h.mem_pct),
    disk: num(h.disk_pct),
    load15: num(h.load15),
    // IOWait is a share of a cumulative CPU-time counter and needs a rate
    // across two points. The fleet query does not compute one, so this is
    // absent rather than 0 — an empty cell says "not measured here", and 0
    // would say "this disk is not waiting", which nothing has established.
    iowait: NaN,
    // Counts of what the agent is holding in memory right now. They describe
    // an agent's buffer, not a host, so there is nothing for the backend to
    // report and the detail view is where they belong.
    mounts: [],
    seriesCount: 0,
    logsCount: 0,
    spansCount: 0,
    dropped: 0,
    pending: [],
  };
}

// useBackendFleet polls the backend's host inventory.
//
// `enabled` matters less than it does for the agent fleet poll — this is one
// small request regardless of host count, which is the point — but it is kept
// so a view that is not on screen is not polling.
export function useBackendFleet(intervalMs = 10000, enabled = true, windowSpec = "10m") {
  const [rows, setRows] = useState([]);
  const [error, setError] = useState(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!enabled) return undefined;

    let cancelled = false;
    const controller = new AbortController();

    async function tick() {
      try {
        const data = await getJSON(`/api/hosts?window=${encodeURIComponent(windowSpec)}`, controller.signal);
        if (cancelled) return;
        // `now` comes from the backend rather than the browser. Host age is
        // the difference between two timestamps, and taking one from each
        // machine measures their clock skew as well as the host's silence —
        // enough on a laptop that has been asleep to mark a live fleet dead.
        const nowMs = typeof data.now === "number" ? data.now : Date.now();
        setRows((data.hosts || []).map((h) => backendHostRow(h, nowMs)));
        setError(null);
      } catch (err) {
        if (cancelled || err.name === "AbortError") return;
        // Keeps the last good rows on screen and marks them stale, for the
        // same reason the agent poll does: blanking the table on one dropped
        // request throws away what you were reading exactly when something
        // has started going wrong.
        setError({ message: err.message, detail: err.detail || "" });
      } finally {
        if (!cancelled) setLoading(false);
      }
    }

    tick();
    const id = setInterval(tick, intervalMs);
    return () => {
      cancelled = true;
      controller.abort();
      clearInterval(id);
    };
  }, [intervalMs, enabled, windowSpec]);

  return { rows, error, loading };
}
