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

## Pointing it at a remote host

The dashboard binds loopback on the agent's side for the same reason, so reach
a remote agent through an SSH forward rather than by rebinding it:

```bash
scripts/dev-tunnel.sh -i ~/.ssh/key.pem ubuntu@203.0.113.10
AGENT_I_URL=http://127.0.0.1:8089 npm run dev
```

The script reconnects on drop. A plain `ssh -N` forward has no keepalive, so an
idle connection gets dropped and stays dead — and the UI then shows "agent not
reachable" until someone notices. When that does happen, the error names the
layer that failed: a proxy that cannot reach its upstream is reported as such,
never as an error from the agent, which returns 200 on this endpoint or
nothing at all.

## Watching several servers at once

The host list lives in the browser and is edited in the UI — the picker in the
header, then the button beside it. Add a server while the dashboard is running;
no restart, and no rebuild.

For a handful of servers, especially across different accounts, bring the
tunnels up together:

```bash
cat > hosts.txt <<'EOF'
# name        ssh target                identity (optional)
prod-web-1    ec2-user@54.209.31.122    ~/.ssh/prod.pem
prod-web-2    ec2-user@54.210.11.9      ~/.ssh/prod.pem
staging-api   ubuntu@18.201.4.7         ~/.ssh/staging.pem
EOF

scripts/dev-tunnels.sh hosts.txt
```

Local ports are assigned in order from 8089, and the script prints the list in
the format the host manager accepts — paste it in and every server appears. The
per-host identity column is what makes several AWS accounts work from one file.

`AGENT_I_HOSTS` still works and still takes the same `name=url` form, but it
only seeds an empty list on first run; after that the browser's copy wins.
"Restore from AGENT_I_HOSTS" in the manager goes back to it.

Two hosts or more turns on the Fleet view: one row per server, sortable, with
instance id, type and zone read from each host's own cloud metadata. Everything
else — metrics, traces, logs — stays one host at a time, because each agent
answers only for itself. A genuinely aggregated view needs a backend that
collects from all of them, which is what the `otlp_http` exporter is for.

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
