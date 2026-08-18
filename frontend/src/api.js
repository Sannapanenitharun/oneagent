import { useEffect, useRef, useState } from "react";

// The upstream the dev server proxies /api to, injected at config time so an
// error can name the address that actually failed instead of saying "the
// agent" and leaving you to guess which one.
const TARGET = typeof __AGENT_TARGET__ === "string" ? __AGENT_TARGET__ : "the agent";

// The agents this UI can switch between, from AGENT_I_HOSTS — see
// vite.config.js. Always at least one entry, so nothing downstream has to
// handle an empty list.
export const HOSTS =
  typeof __AGENT_HOSTS__ !== "undefined" && Array.isArray(__AGENT_HOSTS__) && __AGENT_HOSTS__.length > 0
    ? __AGENT_HOSTS__
    : [{ name: "", url: TARGET }];

// Each host is proxied under its own prefix rather than fetched directly:
// the agent serves no CORS headers on purpose, so a cross-origin fetch from
// the page would fail even with the tunnel up.
const hostPath = (i) => `/h/${i}`;

function targetOf(i) {
  return HOSTS[i]?.url || TARGET;
}

// Why this distinguishes failure kinds at all:
//
// The agent's /api/snapshot handler always writes 200 with a JSON body. It has
// no path that produces an error status — a failed encode is logged on the
// host, not surfaced as a code. So a non-OK status on this endpoint did NOT
// come from the agent; it came from the dev-server proxy in front of it, which
// turns a refused upstream connection into a 500.
//
// Reporting that as "agent returned HTTP 500" points you at a healthy agent
// and away from the actual cause, which is usually a dead SSH tunnel or a
// stopped container. Naming the layer that failed is the whole point.
export class SnapshotError extends Error {
  constructor(kind, message, detail) {
    super(message);
    this.name = "SnapshotError";
    this.kind = kind; // "offline" | "unreachable" | "malformed"
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

export async function fetchSnapshot(signal, hostIndex = 0) {
  const TARGET = targetOf(hostIndex);
  let res;
  try {
    res = await fetch(`${hostPath(hostIndex)}/api/snapshot`, { cache: "no-store", signal });
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
    throw new SnapshotError(
      "unreachable",
      "agent not reachable",
      `The dev server proxy could not reach ${TARGET} and returned HTTP ${res.status}. ` +
        `The agent never returns an error status on this endpoint, so this is a connection ` +
        `failure in front of it — a stopped agent, or a closed SSH tunnel if ${TARGET} is forwarded.`
    );
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
export function useSnapshot(intervalMs = 5000, hostIndex = 0) {
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
      if (pausedRef.current) return;
      try {
        const snap = await fetchSnapshot(controller.signal, hostIndex);
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
  }, [intervalMs, hostIndex]);

  return { snapshot, error, loading, paused, setPaused };
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
export function useHostHealth(intervalMs = 20000) {
  const [health, setHealth] = useState(() => HOSTS.map(() => "unknown"));

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();

    async function probe() {
      const results = await Promise.all(
        HOSTS.map(async (_, i) => {
          try {
            const res = await fetch(`${hostPath(i)}/healthz`, {
              cache: "no-store",
              signal: controller.signal,
            });
            // res.ok alone is not enough. An unproxied path falls through to
            // the dev server's SPA handler, which answers 200 with index.html
            // — so a host with no route would read as healthy. The agent's
            // own /healthz is text/plain "ok", never HTML. Same hazard the
            // snapshot fetch already guards against below.
            if (!res.ok) return "down";
            const type = res.headers.get("content-type") || "";
            return type.includes("text/html") ? "down" : "up";
          } catch {
            // Includes the abort on unmount, which the cancelled guard below
            // discards rather than rendering as every host going down.
            return "down";
          }
        })
      );
      if (!cancelled) setHealth(results);
    }

    probe();
    const id = setInterval(probe, intervalMs);
    return () => {
      cancelled = true;
      controller.abort();
      clearInterval(id);
    };
  }, [intervalMs]);

  return health;
}
