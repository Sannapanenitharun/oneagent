import { useEffect, useRef, useState } from "react";

// Single source of truth for the agent's shape. Everything the UI renders
// comes from this one endpoint — the agent has no other API — so there is one
// fetch, one poll interval, and one error path rather than nine.
export async function fetchSnapshot(signal) {
  const res = await fetch("/api/snapshot", { cache: "no-store", signal });
  if (!res.ok) throw new Error(`agent returned HTTP ${res.status}`);
  return res.json();
}

// Polls rather than streams: the agent has no push channel, and at a 5s
// cadence over loopback the payload is small enough that adding a websocket
// would be complexity without benefit.
export function useSnapshot(intervalMs = 5000) {
  const [snapshot, setSnapshot] = useState(null);
  const [error, setError] = useState(null);
  const [loading, setLoading] = useState(true);
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();

    async function tick() {
      if (pausedRef.current) return;
      try {
        const snap = await fetchSnapshot(controller.signal);
        if (cancelled) return;
        setSnapshot(snap);
        setError(null);
      } catch (err) {
        // An aborted fetch is this component unmounting, not a failure.
        if (cancelled || err.name === "AbortError") return;
        // Keep the last good snapshot on screen and mark it stale. Blanking
        // the dashboard on one dropped poll would throw away the data you
        // were looking at precisely when something is going wrong.
        setError(err.message);
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
  }, [intervalMs]);

  return { snapshot, error, loading, paused, setPaused };
}
