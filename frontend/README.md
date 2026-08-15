# Agent-I frontend

A React UI for a single Agent-I instance. Every view reads from the agent's
one endpoint, `GET /api/snapshot`; there is no other API and no separate
database.

## Run it

The agent must be running with the dashboard enabled:

```yaml
dashboard:
  enabled: true
  listen_addr: "127.0.0.1:8088"
```

Then:

```bash
cd frontend
npm install
npm run dev          # http://localhost:5173
```

Vite proxies `/api` to `http://127.0.0.1:8088`. Point it elsewhere with:

```bash
AGENT_I_URL=http://192.168.1.50:8088 npm run dev
```

The proxy exists so the browser stays same-origin. The agent's dashboard
endpoint deliberately sends no CORS headers — it is an unauthenticated
loopback debug surface, and permissive CORS would let any page you happen to
visit read this host's metrics, logs and trace contents.

## What is live and what is not

Everything is derived client-side in `src/adapters.js` from the raw snapshot.
The agent stays a collector; percentiles, health thresholds and topology are
product judgements that live here, where changing one does not mean
redeploying to every host.

| View | Source | Requires |
|---|---|---|
| Overview | spans + host metrics | `metrics.enabled`, `traces.enabled` |
| Service Topology | span `parent_id` links | spans from ≥2 services |
| Traces | spans, tree rebuilt from `parent_id` | `traces.enabled` |
| Logs | tailed lines | `logs.enabled` + a matching path |
| Metrics | `system.*` series | `metrics.enabled` |
| Infrastructure | `system.*` series | `metrics.enabled` |
| Problems | — | a correlation/root-cause service |
| Exceptions | — | span-event extraction in the receiver |
| Monitors | — | an alerting rule engine |

The last three render an explicit "not available" panel naming the missing
capability rather than showing mock data. A dashboard that invents numbers is
worse than one that admits a gap.

Two known limits, both honest rather than hidden:

- **Log severity is classified in the browser** from the line text. The agent
  forwards log lines verbatim and does not parse levels — imposing one log
  format on every deployment is the wrong call for a collector.
- **Logs carry no trace ID**, so there is no log↔trace jump. That needs the
  application to emit `trace_id` into its log line and the agent to parse it.

## Theming

Light and dark, switched from the header. Three states, not two: **System**
follows the OS and is the default, **Light** and **Dark** pin a choice. The
selection persists in `localStorage`.

All colour lives in `src/index.css` as tokens. Components reference
`var(--token)` and never a literal, so nothing has to be re-rendered when the
theme changes — CSS re-resolves the variables at paint time, including inside
the Recharts SVG.

The rule that keeps it correct: every token is defined at bare `:root` first
and only *redefined* under `@media (prefers-color-scheme: dark)` and
`:root[data-theme="dark"]`. The media query is guarded with
`:root:not([data-theme="light"])` so an explicit Light choice still wins on a
dark OS. A colour whose only definition sits inside a media query never applies
in the un-stamped "system" state, which is how a page ends up rendering one
theme's text on the other theme's background.

Each theme has its own six-hue categorical palette for service colours. The
same hex cannot serve both grounds — a colour bright enough to read on
near-black is too pale on white. Both sets were checked for colour-vision
separation, chroma, lightness band and contrast against their own surface,
and are ordered so no two adjacent slots collide (blue and violet are kept
apart; they are the pair that fails first under deuteranopia). The palette
that shipped with the original mock had violet and blue at ΔE 1.3 under
deuteranopia — effectively the same colour — which is why the hues changed.

Status colours (good / warning / critical) are deliberately separate from the
series palette: a hue that means "critical" must never also mean "series 4".

## Build for production

```bash
npm run build        # -> frontend/dist
```

`dist/` is a plain static bundle. Serving it from the agent binary would mean
embedding it with `go:embed`; the agent currently serves its own simpler
built-in page at `/` instead.
