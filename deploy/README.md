# The backend

Agents push here; the dashboard reads from here. One place holds every host,
so there is no tunnel per server and no aggregation in a browser tab.

```
agent ──┐
agent ──┼── OTLP/HTTP :4318 ──▶ server ──▶ ClickHouse
agent ──┘                          │
                                   └── /api/… ──▶ dashboard
```

The direction is the whole point. Agents connect **outbound** over one port, so
adding the fiftieth server is identical to adding the second — no inbound
rules, no NAT traversal, no per-host wiring, and cross-account is a non-event.

This file is about the **backend**. For running the agent as a container to
monitor a Docker host, see [DOCKER.md](DOCKER.md) — that is a different process
with a different lifecycle: one backend serves many hosts, and an agent runs on
each host that has containers worth watching.

## Run it

```bash
cd deploy
AGENTI_API_KEYS="acct-a:$(openssl rand -hex 24)" docker compose up -d
docker compose logs -f server
```

On Windows, in PowerShell — `VAR=value command` is bash syntax and PowerShell
reads it as the name of a command, so set the variable first:

```powershell
cd deploy
$bytes = New-Object byte[] 24
[System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
$env:AGENTI_API_KEYS = "acct-a:$(($bytes | ForEach-Object { '{0:x2}' -f $_ }) -join '')"
docker compose up -d
docker compose logs -f server
```

`RandomNumberGenerator` rather than `Get-Random`, which is seeded from the
clock and is not meant for anything anyone has to keep secret.

Either way the variable lives in that one shell. Put it in `deploy/.env` — which
is gitignored, and which compose reads on its own — to survive a new terminal.

ClickHouse binds to loopback; only the server's `:4318` is exposed, because
that is the only port anything outside this host needs.

## Point an agent at it

In `/etc/agent-i/agent.yaml`:

```yaml
exporter:
  type: "otlp_http"
  endpoint: "http://<backend-host>:4318"
  headers_env:
    x-api-key: AGENT_I_INGEST_KEY
```

and the key itself in `/etc/agent-i/env`:

```
AGENT_I_INGEST_KEY=<the key you generated>
```

`headers_env` rather than `headers` so the secret is never in the YAML. Then
`sudo systemctl restart agent-i`.

Nothing else is per-host. The agent already names itself from its EC2 Name tag
or hostname and tags its telemetry with `host.id`, `os.*` and `cloud.*`, which
is what makes hosts distinguishable once they arrive.

## Check it works

```bash
curl -s localhost:4318/healthz                       # also checks ClickHouse
curl -s localhost:4318/api/hosts | jq '.hosts[] | {host_id, agent_id, cpu_pct}'
```

## Endpoints

| Path | Purpose |
|---|---|
| `POST /v1/metrics`, `/v1/logs`, `/v1/traces` | OTLP ingest, JSON or protobuf, gzip optional |
| `GET /api/hosts?window=10m` | fleet inventory with latest CPU/memory/disk |
| `GET /api/series?host=&name=&window=&step=` | one metric, bucketed server-side |
| `GET /api/snapshot?host=&window=` | one host's series, logs and spans, in the agent's own payload shape |
| `GET /api/metrics/names?host=` | what a host has reported |
| `GET /healthz` | process **and** database |

The ingest paths are OTLP's own, so any OTLP producer can send here — not only
this project's agent. That is most of the value of having picked OTLP as the
wire format.

## What fills the fleet columns

`GET /api/hosts` reads specific metric names. A name that nothing emits shows
as an empty cell, not an error, so it is worth being explicit:

| Column | Read from | Notes |
|---|---|---|
| CPU | `host.cpu.used_pct`, else `system.cpu.utilization` | the semconv name is a 0..1 ratio and is scaled by 100 |
| Memory | `host.memory.used_pct`, else `system.memory.utilization` | same |
| Disk | `system.filesystem.usage` where `mountpoint=/` | bytes, split `state=used`/`state=free`; the percentage is computed from the pair |

There is no disk *percentage* metric anywhere in the agent — the hostmetrics
collector reports filesystem bytes, which is what the OpenTelemetry receiver
does — so the fleet table derives it. The root filesystem is used because it
is the one every host has and the one "disk" means unqualified.

An empty cell means the host has not reported that metric inside the window.
It is distinct from `0`, which is a real reading: an idle machine shows 0%
CPU, not a blank. `internal/store/query_test.go` pins both cases.

## Configuration

Everything has a working default except the API keys.

| Variable | Default | Notes |
|---|---|---|
| `AGENTI_LISTEN` | `0.0.0.0:4318` | |
| `AGENTI_CLICKHOUSE` | `http://127.0.0.1:8123` | the HTTP port, not the native 9000 |
| `AGENTI_CLICKHOUSE_DB` | `agenti` | created and migrated at startup |
| `AGENTI_CLICKHOUSE_USER` | `default` | |
| `AGENTI_CLICKHOUSE_PASSWORD` | — | environment only, never a flag: flags are visible in `ps` |
| `AGENTI_API_KEYS` | — | `label:key,label:key`. **Unset disables authentication** |
| `AGENTI_BATCH_ROWS` | `10000` | rows buffered before a flush |
| `AGENTI_MAX_SERIES` | `400` | distinct series one host's snapshot carries; capped at 5000 |
| `AGENTI_PORT` | `4318` | published port only — compose reads it; the container always listens on 4318 |

Leaving `AGENTI_API_KEYS` unset is right for a laptop and wrong for anything
with a route to it. The server logs a warning at startup in that case rather
than accepting the world quietly.

`AGENTI_MAX_SERIES` decides when the dashboard shows a truncated view. The
default is a judgement about the browser — 400 series at the point limit is
roughly a megabyte of JSON per poll — not about the host, and a container host
can exceed it honestly: twenty containers reporting eleven metric families each
is over two hundred series before the host's own network and disk devices are
counted. When the cap is reached the snapshot keeps a share of every metric
name rather than the first N alphabetically, reports the shortfall as
`series_dropped`, and the dashboard says so. Raising this accepts a larger
payload; narrowing what the agent collects (`metrics.network.exclude`) is
usually the cheaper fix, because most of the excess is one series per veth.

## Seeing it in the dashboard

The dashboard's Fleet view reads this backend, so a host appears there because
it reported — not because the browser can reach it. Run the two together:

```bash
cd deploy && docker compose up -d      # backend on 4318 (or $AGENTI_PORT)
cd ../frontend && npm run dev          # dashboard on 5173
```

The dev server proxies `/b` to the backend; point it elsewhere with
`AGENT_I_BACKEND_URL`. **A change to `vite.config.js` needs the dev server
restarted** — Vite reloads its own config, but a server started before the
route existed will not have it.

Fleet appears in the sidebar once the backend reports more than one host. Open
any row, or pick a host from the header dropdown under "Via backend", and every
view works: metrics, logs, traces, the waterfall, the service map.

That works because `/api/snapshot` returns the **agent's own payload shape**,
assembled from stored rows. The dashboard's adapters, percentiles and views are
written against that shape, so a host this browser has no route to renders
through exactly the code that renders one it does. A second payload shape would
have meant all of it existing twice and drifting apart from the first day.

What is different when reading the backend: numbers are as fresh as the last
export the host sent rather than as fresh as a poll of it, the window is the
retention rather than the agent's in-memory 15 minutes, and Pause is disabled
because there is no live poll to pause. The header says "via backend" so you
are never guessing which one you are looking at.

## Retention

Set by TTL on each table, applied at migration: **30 days** for metrics,
**15 days** for logs and spans. Partitioning is by day, so expiry is a
partition drop rather than a rewrite. Change the `TTL` clauses in
`internal/store/schema.go` and `ALTER TABLE … MODIFY TTL` to apply it to data
already stored.

## Upgrading an existing database

The `hosts` table's ordering key changed to include `attr_count`. Without it,
an export carrying a thin resource — an application on the host sending traces
tagged with `host.id` and nothing else — permanently erases that machine's OS,
account and zone when ClickHouse next merges the part. The server refuses to
start against the old key and prints the fix:

```sql
DROP TABLE agenti.hosts
```

Dropping it loses nothing durable: the table is rebuilt from the next export
each agent sends, which is seconds.

## What this is not, yet

- **No downsampling.** Raw points are kept for the full retention. Fine at this
  scale; a materialized view rolling up to per-minute would be the next step.
- **No multi-tenancy.** API keys authenticate, they do not isolate: every key
  can query every host. Real separation means a tenant column in the ordering
  key, not a filter in the query layer.
- **No alerting.**
- **Single node.** ClickHouse replication is a deployment change, not a code
  change, but nothing here has been tested against a cluster.
