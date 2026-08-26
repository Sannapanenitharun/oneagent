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

## Run it

```bash
cd deploy
AGENTI_API_KEYS="acct-a:$(openssl rand -hex 24)" docker compose up -d
docker compose logs -f server
```

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
| `GET /api/metrics/names?host=` | what a host has reported |
| `GET /healthz` | process **and** database |

The ingest paths are OTLP's own, so any OTLP producer can send here — not only
this project's agent. That is most of the value of having picked OTLP as the
wire format.

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

Leaving `AGENTI_API_KEYS` unset is right for a laptop and wrong for anything
with a route to it. The server logs a warning at startup in that case rather
than accepting the world quietly.

## Retention

Set by TTL on each table, applied at migration: **30 days** for metrics,
**15 days** for logs and spans. Partitioning is by day, so expiry is a
partition drop rather than a rewrite. Change the `TTL` clauses in
`internal/store/schema.go` and `ALTER TABLE … MODIFY TTL` to apply it to data
already stored.

## What this is not, yet

- **No downsampling.** Raw points are kept for the full retention. Fine at this
  scale; a materialized view rolling up to per-minute would be the next step.
- **No multi-tenancy.** API keys authenticate, they do not isolate: every key
  can query every host. Real separation means a tenant column in the ordering
  key, not a filter in the query layer.
- **No alerting.**
- **Single node.** ClickHouse replication is a deployment change, not a code
  change, but nothing here has been tested against a cluster.
