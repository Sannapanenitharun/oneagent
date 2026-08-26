import { useEffect, useRef, useState } from "react";

// The upstream the dev server proxies /api to, injected at config time so an
// error can name the address that actually failed instead of saying "the
// agent" and leaving you to guess which one.
const TARGET = typeof __AGENT_TARGET__ === "string" ? __AGENT_TARGET__ : "the agent";

// Every request goes through the dev server's /h route, which forwards to the
// address in this header. The agent serves no CORS headers on purpose, so the
// page cannot fetch it directly; and the target rides in a header rather than
// being a fixed proxy route so hosts can be added while the UI is running.
// See vite.config.js.
function agentFetch(host, path, init = {}) {
  return fetch(`/h${path}`, {
    ...init,
    cache: "no-store",
    headers: { ...(init.headers || {}), "X-Agent-Target": host.url },
  });
}

// The proxy reports why it could not reach an agent in a JSON body. Reading it
// is what separates "the tunnel is down" from "the agent is stopped" — a bare
// status code makes those identical, and they have different fixes.
async function proxyDetail(res) {
  try {
    const body = await res.clone().json();
    if (body && typeof body.error === "string") return body.error;
  } catch {
    /* not our JSON; fall back to the generic wording */
  }
  return "";
}

// Why this distinguishes failure kinds at all:
//
// The agent's /api/snapshot handler always writes 200 with a JSON body, and
// has no path that produces an error status — a failed encode is logged on the
// host, not surfaced as a code. So a non-OK status on this endpoint did not
// come from the agent.
//
// It does not follow that it came from the proxy. This originally assumed it
// did, and told anyone who hit a 404 to go and check their SSH tunnel — while
// the actual cause was another process sitting on the port they had typed,
// answering happily and knowing nothing about /api/snapshot. A tunnel that is
// fine is an expensive thing to be sent to debug.
//
// So the layer is identified rather than assumed: the proxy reports its own
// failures as a JSON body carrying `error`, and anything else is an answer
// from whatever is actually listening. See describeSnapshotFailure.
export class SnapshotError extends Error {
  constructor(kind, message, detail) {
    super(message);
    this.name = "SnapshotError";
    // "offline"      — this page cannot reach its own origin
    // "unreachable"  — nothing usable answered at the target
    // "not-an-agent" — something answered, but it is not an agent
    // "unauthorized" — an agent answered and refused
    // "malformed"    — a 200 that was not the payload
    this.kind = kind;
    this.detail = detail;
  }
}

// Whether the origin serving this page is still answering. Used only to
// interpret a failure: the dev server proxies /api but serves / itself, so if
// / responds and /api does not, the fault is upstream of the proxy.
async function originAlive(signal) {
  try {
    await fetch("/", { method: "HEAD", cache: "no-store", signal });
    return true;
  } catch {
    return false;
  }
}

// describeSnapshotFailure works out which layer produced a non-OK response.
//
// The distinction is the whole value of the message. The dev-server proxy
// reports its own failures as a JSON body carrying `error` — a refused
// connection, a timeout, a dead tunnel. Anything else came back from whatever
// is listening at that address, which means something IS there and answering,
// and the interesting question changes from "why can I not connect" to "what
// am I connected to".
//
// A 404 is the common shape of that: a port on this machine that belongs to a
// different process entirely. The agent serves /api/snapshot and cannot 404
// it, so a 404 is positive evidence of the wrong address rather than of a
// broken one — and telling someone to check their SSH tunnel when the real
// problem is that they typed the port of another service sends them to debug
// a tunnel that is fine.
//
// Pure, so the classification can be tested without a server. See
// verify-api.mjs.
export function describeSnapshotFailure(status, proxyError, target) {
  if (proxyError) {
    return {
      kind: "unreachable",
      message: "agent not reachable",
      detail: `The dev server proxy could not reach ${target}: ${proxyError}`,
    };
  }
  if (status === 404) {
    return {
      kind: "not-an-agent",
      message: "not an agent",
      detail:
        `Something is listening at ${target} and it answered HTTP 404 for /api/snapshot. ` +
        `The agent serves that path and never 404s it, so this is a different process on ` +
        `that port — check the address rather than the tunnel.`,
    };
  }
  if (status === 401 || status === 403) {
    return {
      kind: "unauthorized",
      message: "agent refused the request",
      detail:
        `${target} answered HTTP ${status}. The agent's dashboard has an auth token ` +
        `configured and this page is not sending it.`,
    };
  }
  return {
    kind: "unreachable",
    message: "agent not reachable",
    detail:
      `${target} answered HTTP ${status}, which the agent does not return for this path. ` +
      `Something is listening there, so this is more likely the wrong address than a dead tunnel.`,
  };
}

// isAgentHealthz reports whether a /healthz response came from an agent.
//
// Status alone is not enough, and neither is "200 and not HTML". Any service
// that happens to occupy the port can answer 200 text/plain — an ingest
// endpoint, a metrics exporter, another project's daemon — and the health dot
// exists precisely to stop you switching to a host that will not load. A dot
// that goes green for the wrong process is worse than no dot: it moves the
// confusion from the picker to the dashboard.
//
// The agent's /healthz returns the literal string "ok" and is documented to,
// so that is what is checked.
export function isAgentHealthz(ok, contentType, body) {
  if (!ok) return false;
  if ((contentType || "").includes("text/html")) return false;
  return String(body || "").trim().toLowerCase() === "ok";
}

export async function fetchSnapshot(signal, host) {
  const TARGET = host?.url || "the agent";
  let res;
  try {
    res = await agentFetch(host, "/api/snapshot", { signal });
  } catch (err) {
    // An aborted request is a caller concern, so it passes through.
    if (err.name === "AbortError") throw err;

    // A rejected fetch carries no status. Usually that means this page's own
    // origin is gone, because a refused upstream comes back as a 500 handled
    // below — measured against a live dev server with a dead proxy target, not
    // assumed. But the proxy can also drop the connection outright (an upstream
    // that accepts then resets, which is what a half-open SSH tunnel does), and
    // that lands here too. Probing a path the dev server answers itself is what
    // separates the two; blaming the dev server for an unreachable agent sends
    // you to restart a healthy process.
    if (await originAlive(signal)) {
      throw new SnapshotError(
        "unreachable",
        "agent not reachable",
        `The dev server is running but could not connect to ${TARGET}. The agent is stopped, ` +
          `or — if ${TARGET} is a forwarded port — the SSH tunnel is down.`
      );
    }
    throw new SnapshotError(
      "offline",
      "dev server not responding",
      "The page could not reach its own origin. Is `npm run dev` still running?"
    );
  }

  if (!res.ok) {
    const detail = await proxyDetail(res);
    const f = describeSnapshotFailure(res.status, detail, TARGET);
    throw new SnapshotError(f.kind, f.message, f.detail);
  }

  // A proxy misconfiguration can return 200 with the SPA's own index.html,
  // which fails as JSON in a way that reads like corrupt data from the agent.
  try {
    return await res.json();
  } catch {
    throw new SnapshotError(
      "malformed",
      "unexpected response",
      `${TARGET} answered with ${res.headers.get("content-type") || "an unknown content type"} ` +
        `instead of JSON. That usually means /api is not proxied to an agent.`
    );
  }
}

// Polls rather than streams: the agent has no push channel, and at a 5s
// cadence over loopback the payload is small enough that adding a websocket
// would be complexity without benefit.
export function useSnapshot(intervalMs = 5000, host = null) {
  const [snapshot, setSnapshot] = useState(null);
  const [error, setError] = useState(null);
  const [loading, setLoading] = useState(true);
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();

    // Switching hosts must clear the previous one's data rather than leaving
    // it on screen under the new host's name. The alternative — keeping the
    // last good snapshot, which is right for a dropped poll — would here be
    // showing one machine's metrics labelled as another's.
    setSnapshot(null);
    setError(null);
    setLoading(true);

    async function tick() {
      if (pausedRef.current || !host?.url) return;
      try {
        const snap = await fetchSnapshot(controller.signal, host);
        if (cancelled) return;
        setSnapshot(snap);
        setError(null);
      } catch (err) {
        // An aborted fetch is this component unmounting, not a failure.
        if (cancelled || err.name === "AbortError") return;
        // Keep the last good snapshot on screen and mark it stale. Blanking
        // the dashboard on one dropped poll would throw away the data you
        // were looking at precisely when something is going wrong.
        setError({
          kind: err.kind || "unreachable",
          message: err.message,
          detail: err.detail || "",
        });
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
    // Keyed on the URL, not the object: the host list is rebuilt on every edit,
    // so a new object identity for an unchanged address would restart the poll
    // and blank the dashboard every time an unrelated host was renamed.
  }, [intervalMs, host?.url]);

  return { snapshot, error, loading, paused, setPaused };
}

// useAllSnapshots polls every configured host, for the fleet view.
//
// `enabled` is not an optimisation detail, it is the point: this multiplies
// transfer by the host count, and each snapshot is the agent's whole retention
// window. Polling all hosts while you are looking at one host's traces would
// be paying that cost for nothing, so the caller passes false whenever the
// fleet view is not on screen.
//
// Slower than the single-host poll for the same reason. A fleet overview is
// read at a glance; it does not need the cadence of a view you are watching.
//
// One host failing must not blank the others — each result carries its own
// error, and Promise.all over already-caught promises never rejects.
export function useAllSnapshots(hosts, intervalMs = 10000, enabled = true) {
  const [results, setResults] = useState(() => hosts.map((h) => ({ host: h, snapshot: null, error: null })));
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!enabled) return undefined;

    let cancelled = false;
    const controller = new AbortController();

    async function tick() {
      const next = await Promise.all(
        hosts.map(async (h) => {
          try {
            return { host: h, snapshot: await fetchSnapshot(controller.signal, h), error: null };
          } catch (err) {
            if (err.name === "AbortError") return null;
            return {
              host: h,
              snapshot: null,
              error: { kind: err.kind || "unreachable", message: err.message, detail: err.detail || "" },
            };
          }
        })
      );
      if (cancelled || next.some((r) => r === null)) return;
      setResults(next);
      setLoading(false);
    }

    tick();
    const id = setInterval(tick, intervalMs);
    return () => {
      cancelled = true;
      controller.abort();
      clearInterval(id);
    };
    // Joined rather than passed as an array for the same reason useSnapshot
    // keys on the URL: a fresh array on every render would re-run this effect
    // continuously and never complete a poll.
  }, [intervalMs, enabled, hosts.map((h) => h.url).join(",")]);

  return { results, loading };
}

// useHostHealth reports reachability for EVERY configured host, so the picker
// can show which are up before you switch to one.
//
// Deliberately probes /healthz rather than /api/snapshot: the snapshot is the
// whole 15-minute window and polling it for N hosts would multiply the
// transfer by N to answer a yes/no question. /healthz is a few bytes.
//
// Slow on purpose. This exists to stop you switching to a host whose tunnel
// died, not to be a monitor — the selected host is polled at the normal rate
// and is the one whose state actually matters.
export function useHostHealth(hosts, intervalMs = 20000) {
  // Keyed by URL rather than by position. The list is editable while the UI is
  // running, so a positional array would hand one host's reachability to
  // whichever host inherited its index after a removal — showing a live dot on
  // a dead tunnel, which is the one thing this is here to prevent.
  const [health, setHealth] = useState({});

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();

    async function probe() {
      const probed = await Promise.all(
        hosts.map(async (h) => {
          try {
            const res = await agentFetch(h, "/healthz", { signal: controller.signal });
            // The body is read, not just the status. An unproxied path falls
            // through to the dev server's SPA handler and answers 200 with
            // index.html; any other process on that port can answer 200
            // text/plain. Both would read as healthy on status alone, and a
            // green dot on the wrong process is worse than no dot — it moves
            // the confusion from the picker into the dashboard.
            const body = res.ok ? await res.text() : "";
            const type = res.headers.get("content-type") || "";
            return [h.url, isAgentHealthz(res.ok, type, body) ? "up" : "down"];
          } catch {
            // Includes the abort on unmount, which the cancelled guard below
            // discards rather than rendering as every host going down.
            return [h.url, "down"];
          }
        })
      );
      if (!cancelled) setHealth(Object.fromEntries(probed));
    }

    probe();
    const id = setInterval(probe, intervalMs);
    return () => {
      cancelled = true;
      controller.abort();
      clearInterval(id);
    };
  }, [intervalMs, hosts.map((h) => h.url).join(",")]);

  return health;
}
