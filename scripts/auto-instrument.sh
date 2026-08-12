#!/usr/bin/env bash
# OneAgent auto-instrumentation.
#
# Detects systemd-managed Node.js and Python web services running on this
# host, and — only when explicitly told to — installs OpenTelemetry
# instrumentation into them and points them at OneAgent's trace receiver
# (localhost:4319), so their real requests show up as traces without you
# writing any instrumentation code by hand.
#
# SAFETY DESIGN, read before running with --apply:
#   This script restarts real, possibly production, services that have
#   nothing to do with OneAgent itself. Restarting a live service always
#   carries some risk (a brief connection drop at minimum; a bad
#   interaction with in-flight requests at worst). Because of that:
#     - Default mode is DRY RUN: it detects and prints what it WOULD do,
#       changes nothing, restarts nothing.
#     - Pass --apply to actually make changes.
#     - Only touches services that are systemd-managed — anything started
#       by hand (nohup, a raw shell, tmux) is reported but skipped,
#       because there's no safe, general way to know how to restart it
#       correctly.
#     - Every change is additive and reversible: Node gets a NODE_OPTIONS
#       env var addition (no ExecStart edit), Python gets an ExecStart
#       wrapper via a systemd drop-in file — both undoable by deleting
#       the drop-in file this script creates and reloading systemd.
#
# Usage:
#   sudo ./scripts/auto-instrument.sh              # dry run — detect and report only
#   sudo ./scripts/auto-instrument.sh --apply       # actually instrument + restart
set -euo pipefail

APPLY=false
if [[ "${1:-}" == "--apply" ]]; then
  APPLY=true
fi

ONEAGENT_TRACE_ENDPOINT="http://localhost:4319"
DROPIN_MARKER="oneagent-otel"

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (sudo ./scripts/auto-instrument.sh)" >&2
  exit 1
fi

instrument_node() {
  local unit="$1" cwd="$2" node_exe="$3"
  local bootstrap="$cwd/oneagent-otel-bootstrap.js"
  # Derive npm's path from the SAME node binary this process is actually
  # running, rather than trusting `npm` to be on PATH. This matters
  # specifically because sudo does not inherit the invoking user's PATH
  # by default — a bare `npm` here would fail on any nvm-managed Node
  # install (npm lives in the same versioned bin/ dir as node, which is
  # only on the shell's PATH, never root's). Fall back to bare `npm`
  # only if this specific lookup comes up empty, as a last resort.
  local node_dir npm_bin
  node_dir="$(dirname "$node_exe")"
  if [[ -x "$node_dir/npm" ]]; then
    npm_bin="$node_dir/npm"
  else
    npm_bin="npm"
    echo "    warning: could not find npm alongside $node_exe — falling back to \$PATH, which may fail under sudo"
  fi

  echo "    -> installing OpenTelemetry packages ($npm_bin)"
  # npm's own script is typically `#!/usr/bin/env node` internally — even
  # with npm's absolute path resolved above, it still needs `node`
  # findable via PATH to actually run. Prepend node's directory rather
  # than relying on whatever PATH sudo happened to inherit.
  (cd "$cwd" && PATH="$node_dir:$PATH" "$npm_bin" install --save \
    @opentelemetry/api \
    @opentelemetry/sdk-node \
    @opentelemetry/auto-instrumentations-node \
    @opentelemetry/exporter-trace-otlp-proto >/dev/null)

  echo "    -> writing $bootstrap"
  cat > "$bootstrap" << JSEOF
// Written by oneagent auto-instrument.sh — safe to delete to undo.
// Initializes OpenTelemetry auto-instrumentation and sends traces to
// OneAgent's receiver on this host.
const { NodeSDK } = require('@opentelemetry/sdk-node');
const { getNodeAutoInstrumentations } = require('@opentelemetry/auto-instrumentations-node');
const { OTLPTraceExporter } = require('@opentelemetry/exporter-trace-otlp-proto');

const sdk = new NodeSDK({
  traceExporter: new OTLPTraceExporter({ url: '${ONEAGENT_TRACE_ENDPOINT}/v1/traces' }),
  instrumentations: [getNodeAutoInstrumentations()],
});
sdk.start();

// The trace exporter batches spans and flushes periodically rather than
// on every request — without this handler, spans generated just before
// a systemd restart/stop (which sends SIGTERM) would be silently lost,
// since the process would exit before the next scheduled flush.
process.on('SIGTERM', () => {
  sdk.shutdown().finally(() => process.exit(0));
});
JSEOF

  local dropin_dir="/etc/systemd/system/${unit}.d"
  echo "    -> writing systemd drop-in at $dropin_dir/$DROPIN_MARKER.conf"
  mkdir -p "$dropin_dir"
  cat > "$dropin_dir/$DROPIN_MARKER.conf" << CONFEOF
[Service]
Environment=NODE_OPTIONS=--require=$bootstrap
Environment=OTEL_SERVICE_NAME=$unit
CONFEOF

  echo "    -> reloading systemd and restarting $unit"
  systemctl daemon-reload
  systemctl restart "$unit"
  echo "    done."
}

instrument_python() {
  local unit="$1" pip_bin="$2" cmdline="$3"
  local bin_dir
  bin_dir="$(dirname "$pip_bin")"

  echo "    -> installing OpenTelemetry packages (pip)"
  "$pip_bin" install --quiet opentelemetry-distro opentelemetry-exporter-otlp-proto-http

  echo "    -> auto-installing instrumentation for detected libraries"
  "$bin_dir/opentelemetry-bootstrap" -a install >/dev/null

  local dropin_dir="/etc/systemd/system/${unit}.d"
  echo "    -> writing systemd drop-in at $dropin_dir/$DROPIN_MARKER.conf"
  mkdir -p "$dropin_dir"
  cat > "$dropin_dir/$DROPIN_MARKER.conf" << CONFEOF
[Service]
ExecStart=
ExecStart=$bin_dir/opentelemetry-instrument $cmdline
Environment=OTEL_EXPORTER_OTLP_ENDPOINT=$ONEAGENT_TRACE_ENDPOINT
Environment=OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
Environment=OTEL_SERVICE_NAME=$unit
CONFEOF

  echo "    -> reloading systemd and restarting $unit"
  systemctl daemon-reload
  systemctl restart "$unit"
  echo "    done."
}

echo "==> scanning systemd-managed services for instrumentable apps"
echo "    mode: $([ "$APPLY" = true ] && echo 'APPLY (will install packages and restart services)' || echo 'DRY RUN (detect + report only, nothing will change)')"
echo ""

FOUND_ANY=false

for unit in $(systemctl list-units --type=service --state=running --no-legend --plain | awk '{print $1}'); do
  pid="$(systemctl show -p MainPID --value "$unit" 2>/dev/null || true)"
  if [[ -z "$pid" || "$pid" == "0" ]]; then
    continue
  fi
  if [[ ! -r "/proc/$pid/cmdline" ]]; then
    continue
  fi
  # cmdline is NUL-separated; convert to space-separated for matching/display.
  cmdline="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
  # Resolve the actual executable via /proc/<pid>/exe rather than trusting
  # /proc/<pid>/comm — comm reflects the CURRENT thread name, which many
  # Node.js apps rename at runtime (worker_threads, APM/monitoring
  # libraries like dd-trace, native addons via prctl) even though the
  # process is genuinely running node. exe is a symlink to the actual
  # binary on disk and can't be renamed this way.
  exe_path="$(readlink -f "/proc/$pid/exe" 2>/dev/null || true)"
  exe_name="$(basename "$exe_path" 2>/dev/null || true)"

  # --- Node.js detection ---
  if [[ "$exe_name" == "node" ]]; then
    cwd="$(readlink -f "/proc/$pid/cwd" 2>/dev/null || true)"
    if [[ -n "$cwd" && -f "$cwd/package.json" ]]; then
      FOUND_ANY=true
      echo "[Node.js] $unit"
      echo "    pid=$pid  cwd=$cwd"
      echo "    cmdline: $cmdline"

      if grep -q "$DROPIN_MARKER" <<<"$(systemctl show -p DropInPaths --value "$unit" 2>/dev/null || true)"; then
        echo "    already instrumented (drop-in present) — skipping"
        echo ""
        continue
      fi

      if [[ "$APPLY" == true ]]; then
        instrument_node "$unit" "$cwd" "$exe_path"
      else
        echo "    would run: npm install (4 OTel packages) in $cwd, using npm from $(dirname "$exe_path")"
        echo "    would write: $cwd/oneagent-otel-bootstrap.js"
        echo "    would create: /etc/systemd/system/$unit.d/$DROPIN_MARKER.conf (NODE_OPTIONS=--require=.../oneagent-otel-bootstrap.js)"
        echo "    would run: systemctl daemon-reload && systemctl restart $unit"
      fi
      echo ""
    fi
  fi

  # --- Python (uvicorn/gunicorn/flask) detection ---
  # NOTE: unlike the Node.js check above, this still matches against
  # cmdline text (*uvicorn*, *gunicorn*, *flask*), which COULD be
  # rewritten by libraries like setproctitle (common in gunicorn/uvicorn
  # workers) the same way comm was for Node — this hasn't been confirmed
  # as an actual problem on a real host the way the comm issue was, but
  # if Python detection ever silently misses a real service, this
  # cmdline-matching assumption is the first thing to check.
  if [[ "$exe_name" == python* ]] && [[ "$cmdline" == *uvicorn* || "$cmdline" == *gunicorn* || "$cmdline" == *flask* ]]; then
    FOUND_ANY=true
    echo "[Python web app] $unit"
    echo "    pid=$pid"
    echo "    cmdline: $cmdline"

    py_bin="$(readlink -f "/proc/$pid/exe" 2>/dev/null || true)"
    pip_bin="$(dirname "$py_bin")/pip"

    if grep -q "$DROPIN_MARKER" <<<"$(systemctl show -p DropInPaths --value "$unit" 2>/dev/null || true)"; then
      echo "    already instrumented (drop-in present) — skipping"
      echo ""
      continue
    fi

    if [[ "$APPLY" == true ]]; then
      instrument_python "$unit" "$pip_bin" "$cmdline"
    else
      echo "    would run: $pip_bin install opentelemetry-distro opentelemetry-exporter-otlp-proto-http"
      echo "    would run: opentelemetry-bootstrap -a install (auto-installs instrumentation for detected libraries)"
      echo "    would create: /etc/systemd/system/$unit.d/$DROPIN_MARKER.conf (wraps ExecStart with opentelemetry-instrument, sets OTEL_EXPORTER_OTLP_ENDPOINT)"
      echo "    would run: systemctl daemon-reload && systemctl restart $unit"
    fi
    echo ""
  fi
done

if [[ "$FOUND_ANY" == false ]]; then
  echo "No systemd-managed Node.js or Python web apps detected."
  echo "(Processes started outside systemd — nohup, a manual shell, tmux — are intentionally not auto-instrumented; see the safety note at the top of this script.)"
fi

if [[ "$APPLY" == false && "$FOUND_ANY" == true ]]; then
  echo "This was a dry run — nothing was changed or restarted."
  echo "Re-run with --apply to actually instrument and restart the services listed above."
fi
