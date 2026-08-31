# Monitoring a Docker host

There are two shapes, and they differ in exactly one thing that matters: what
the agent is allowed to read.

| | Containerised agent (this document) | Host install (`scripts/install.sh`) |
|---|---|---|
| Runs as | root inside the container, no capabilities | `agent-i`, an unprivileged system user |
| Container CPU / memory / IO / PIDs | yes | yes |
| Container network rx/tx | yes (needs `--pid host`) | yes |
| Container names, images, labels | yes (socket mounted) | only with `usermod -aG docker agent-i` |
| Container logs | yes, read from the json-file files | yes, streamed from the Engine API |
| Host `/var/log` files, journald | via bind mounts | natively |

The numbers need nothing privileged in either shape: cgroup files and
`/proc/<pid>/net/dev` are world-readable. The split is over names and logs, both
of which need the Engine socket on a host install — the json-file directory is
mode 0700 and owned by root, so no group grant opens it, and the API is the only
route to container stdout that does not mean running the whole agent as root.
The agent probes for this at startup and says which reader it chose; see
`containers.logs.source` in `configs/agent.yaml`.

The rest of this document is the containerised shape. The agent runs as one
container per Docker host, beside the containers it watches.

```
Docker host
├── app container ─┐
├── app container ─┤   cgroups ──▶ numbers
├── app container ─┤   json-file logs ──▶ stdout/stderr
│                  │   docker.sock ──▶ names
└── agent-i ◀──────┘
        │
        └── OTLP/HTTP ──▶ backend
```

## Run it

```bash
docker run -d --name agent-i \
  --pid host --cgroupns host --uts host \
  -v /proc:/host/proc:ro \
  -v /sys/fs/cgroup:/host/sys/fs/cgroup:ro \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /var/lib/docker/containers:/host/var/lib/docker/containers:ro \
  -v /var/log:/host/var/log:ro \
  -v agent-i-state:/var/lib/agent-i \
  -e AGENT_I_HOST_ROOT=/host \
  -p 4318:4318 \
  agent-i:latest
```

Or `docker compose -f docker-compose.agent.yml up -d`, which is the same thing
with every flag explained inline.

No `--privileged`. No added capabilities. Every host mount read-only. The agent
issues GETs to the Docker socket and opens files for reading — it has no code
path that writes to the host at all.

## What each flag is for, and what breaks without it

| Flag / mount | Buys you | Omitted |
|---|---|---|
| `-e AGENT_I_HOST_ROOT=/host` | Everything below resolves against the host | Agent reads its **own** `/proc` and reports the container's view of the machine as the host's |
| `-v /proc:/host/proc:ro` | Host CPU, memory, load, disk I/O, network; container network counters | No host metrics |
| `-v /sys/fs/cgroup:...:ro` | Every `container.*` number | No container metrics at all |
| `--cgroupns host` | Sibling containers are visible | Agent reports **zero** containers on a host running fifty |
| `--pid host` | Per-container network counters | Container network metrics absent; everything else fine |
| `-v /var/run/docker.sock:...:ro` | Container names, images | Metrics still collected, labelled by 12-char id |
| `-v /var/lib/docker/containers:...:ro` | Container stdout/stderr | No container logs |
| `--uts host` | Stable host identity | Agent names itself after the container id, which changes on every recreate — this host arrives at the backend as a new machine each restart |
| `-v agent-i-state:/var/lib/agent-i` | Read offsets survive a recreate | Restart re-reads or skips log files |
| `-v /var/log:/host/var/log:ro` | Host log files | Nothing matches `logs.paths` |

Two of these deserve the emphasis:

**`--cgroupns host` is not optional.** Under cgroup v2, a container in its own
cgroup namespace sees `/sys/fs/cgroup` rooted at *its own* cgroup — siblings are
invisible no matter what you mount. Without this flag the agent starts cleanly,
reports the host correctly, and finds no containers.

**`AGENT_I_HOST_ROOT` prevents wrong data, not missing data.** An agent reading
its own `/proc/meminfo` gets an answer. It is the container's answer, reported
as the host's.

## What it collects

Metric names follow the OpenTelemetry container semantic conventions — the same
set the OTel Collector's `dockerstats` receiver emits — not Datadog's `container.*`
names, which overlap textually and disagree on units.

| Metric | Type | Source |
|---|---|---|
| `container.cpu.usage.total` / `.usermode` / `.kernelmode` | counter, ns | `cpu.stat` |
| `container.cpu.throttling_data.throttled_time` | counter, ns | `cpu.stat` |
| `container.cpu.utilization` | gauge, % | derived from two samples |
| `container.memory.usage.total` | gauge, bytes | `memory.current` − `inactive_file` |
| `container.memory.usage.limit` | gauge, bytes | `memory.max`, omitted when unlimited |
| `container.memory.percent` | gauge, % | omitted when unlimited |
| `container.memory.file` | gauge, bytes | `memory.stat` |
| `container.blockio.io_service_bytes_recursive` | counter, bytes | `io.stat`, `operation=read\|write` |
| `container.network.io.usage.rx_bytes` / `.tx_bytes` | counter, bytes | `/proc/<pid>/net/dev` |
| `container.pids.count` | gauge | `pids.current` |

Every one carries `container.id`, `container.name`, `container.image.name` and
`container.runtime`. Logs arrive as `container/<name>` with a `stream` attribute
of `stdout` or `stderr`.

Two deliberate choices worth knowing:

- **`container.cpu.utilization` is percent of one CPU**, so a container using
  two cores fully reads 200%. That is what `docker stats` shows, and silently
  disagreeing with it by a factor of *n*CPU would be worse than needing this
  sentence.
- **`container.memory.usage.total` excludes reclaimable page cache.** A
  container that merely read a large file is not using that memory, and the OOM
  killer agrees. Reporting `memory.current` raw is the usual reason container
  memory dashboards show everything pinned near its limit.

## Traces

This is the half of the problem that running the agent as a container solves.

An application container **cannot reach a receiver bound to the host's
`127.0.0.1`** — that is the single most common reason spans never arrive. With
the agent in a container, apps on the same network send to the agent by name:

```yaml
services:
  api:
    environment:
      OTEL_EXPORTER_OTLP_ENDPOINT: "http://agent-i:4318"
      OTEL_SERVICE_NAME: "api"
    networks: [observability]
```

Apps elsewhere on the host use the published port instead. The receiver takes
all three OTLP signals on `4318`, so one endpoint covers traces, metrics and
logs from an instrumented app.

Set `AGENT_I_TRACE_TOKEN` in the agent's environment to require a bearer token.
Worth doing the moment that port is published beyond a private network.

## Ship it somewhere

The image's baked config exports to **stdout** — enough to confirm the agent
works, useless for anything else. Mount your own:

```yaml
exporter:
  type: "otlp_http"
  endpoint: "http://backend:4318"
  headers_env:
    x-api-key: AGENT_I_INGEST_KEY
```

```bash
-v ./agent.yaml:/etc/agent-i/agent.yaml:ro -e AGENT_I_INGEST_KEY=…
```

The key goes in the environment, never in the YAML.

## Requirements and limits

- **cgroup v2 only.** v1 splits each controller into a separate mount whose
  layout differs by cgroup driver, and reading the wrong one attributes another
  container's usage. A v1 host is detected and reported at startup rather than
  silently collecting nothing. Ubuntu 22.04+, Debian 12+, RHEL 9+ and Amazon
  Linux 2023 are all v2; Amazon Linux 2 is not.
- **json-file log driver.** A host using `journald`, `local` or a remote driver
  has no files under `/var/lib/docker/containers`.
- **Runs as root**, unlike the backend image. Container logs are mode 0700
  root-only and other users' cgroup entries are not world-readable, so an
  unprivileged uid would produce an agent that starts cleanly and silently omits
  every container it cannot see. What makes it acceptable is what is *not*
  granted: no `--privileged`, no capabilities, read-only mounts throughout.
- **The agent excludes its own container** from both metrics and logs.
- **journald is off in this image** — `journalctl` is not installed.

## Why this shape

Datadog and Dynatrace ship containerised agents that mean opposite things.

**Datadog's container is the agent.** Unprivileged, read-only bind mounts,
`--cgroupns host --pid host`. Numbers come from cgroups; the socket supplies
only identity. That split is why the same metric code serves containerd and
Podman, and why the socket can be optional.

**Dynatrace's container is an installer.** It mounts the host root at
`/mnt/root`, runs the ordinary full-stack installer, and leaves the real agent
on the host at `/opt/dynatrace/oneagent` — needing fourteen capabilities
including `SYS_ADMIN` and `SYS_PTRACE`, because it injects into running
processes.

This is Datadog's model. Nothing here installs anything on the host or injects
into anything. A fourteen-capability host-root-mount container is not compatible
with "a single static binary that reads files", which is what this agent is.

## Troubleshooting

**No containers reported.** Check the startup line: `containers: reading cgroup
v2 under /host/sys/fs/cgroup`. If it says v1 or "no cgroup filesystem", that is
your answer. If it says v2 and you still see nothing, you are missing
`--cgroupns host`.

**Containers named as hex strings.** The Docker socket is not mounted. Metrics
are fine.

**This host keeps appearing as a new machine.** Missing `--uts host`. Set
`agent_id` in the config instead if you would rather pin it.

**No container logs.** Check the log driver — `docker info -f '{{.LoggingDriver}}'`
must be `json-file` for the file reader. The API reader works with any driver
that supports `docker logs`, and reports the ones that do not (`journald` with
remote storage, `awslogs`, `splunk`) as a named container it cannot read rather
than as silence.

**No container logs on a host install.** Look for the startup line beginning
`container logs:`. `cannot read /var/lib/docker/containers` means the agent is
running unprivileged, as it should be, and has fallen back to the API — which
then needs `sudo usermod -aG docker agent-i && sudo systemctl restart agent-i`.
The docker group is root-equivalent, which is why the installer does not grant
it unless asked with `AGENT_I_ENABLE_DOCKER=1`.

**Host metrics look like the container's.** `AGENT_I_HOST_ROOT` is not set, or
`/proc` is not mounted where it points.
