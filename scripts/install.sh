#!/usr/bin/env bash
# Agent-I installer.
# Usage (from within the agent-i repo/tarball root):
#   sudo ./scripts/install.sh
#
# What it does, in order:
#   1. builds the static Go binary
#   2. creates a dedicated, unprivileged 'agent-i' system user
#   3. installs the binary, default config, and systemd unit
#   4. enables + starts the service
#
# Idempotent: safe to re-run (e.g. after `git pull`) to redeploy a new build.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (sudo ./scripts/install.sh)" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "error: Go toolchain not found. Install Go 1.22+ first: https://go.dev/dl/" >&2
  exit 1
fi

echo "==> building agent-i"
# Stamp the build with the checkout it came from, so `agent-i
# --version` and the telemetry.distro.version resource attribute both
# identify exactly what is deployed. A "-dirty" suffix means the working
# tree had uncommitted changes at build time — that binary corresponds to
# no commit, which is worth knowing when a host reports something the
# committed code cannot produce.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
echo "    version: ${VERSION}"
CGO_ENABLED=0 go build \
  -ldflags "-X github.com/agent-i/agent/internal/version.Version=${VERSION}" \
  -o /tmp/agent-i ./cmd/agent

echo "==> installing binary"
install -m 0755 /tmp/agent-i /usr/local/bin/agent-i

echo "==> installing auto-instrument helper"
install -m 0755 scripts/auto-instrument.sh /usr/local/bin/agent-i-auto-instrument

echo "==> creating system user (if absent)"
id -u agent-i >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --groups adm agent-i
# See get.sh for why this is unconditional (idempotent) rather than only
# on first creation.
usermod -aG adm agent-i
# Read access to the systemd journal, which is owned by this group. Added here
# rather than left to the operator so that turning journald.enabled on is a
# one-line config change — the same group Datadog's installer documents. A host
# without systemd has no such group, and that is not an error worth failing an
# install over.
if getent group systemd-journal >/dev/null 2>&1; then
  usermod -aG systemd-journal agent-i
else
  echo "    no systemd-journal group on this host — journald collection will not be available"
fi

echo "==> installing config"
mkdir -p /etc/agent-i
if [[ ! -f /etc/agent-i/agent.yaml ]]; then
  install -m 0640 -o agent-i -g agent-i configs/agent.yaml /etc/agent-i/agent.yaml
  # The shipped config leaves agent_id empty on purpose: the agent then names
  # itself after the machine at startup (EC2 Name tag, else instance id, else
  # hostname), so an unattended fleet install cannot produce N hosts sharing a
  # single id. AGENT_I_AGENT_ID covers the case where the operator wants a name
  # of their own instead.
  if [[ -n "${AGENT_I_AGENT_ID:-}" ]]; then
    # Restricted to characters that need no YAML quoting or escaping. Anything
    # else is refused rather than written, because a value containing a quote
    # or a backslash would produce a config file the agent cannot parse — and
    # it would fail at the next start, long after this script exited 0.
    if [[ ! "$AGENT_I_AGENT_ID" =~ ^[A-Za-z0-9._-]+$ ]]; then
      echo "error: AGENT_I_AGENT_ID may contain only letters, digits, dot, underscore and hyphen (got: $AGENT_I_AGENT_ID)" >&2
      exit 1
    fi
    # Exact-line match and printf rather than sed: the id is data, never part
    # of a pattern or a replacement string that could reinterpret it.
    agent_id_written=0
    tmp_cfg="$(mktemp)"
    while IFS= read -r line; do
      if [[ "$line" == 'agent_id: ""' ]]; then
        printf 'agent_id: "%s"\n' "$AGENT_I_AGENT_ID"
        agent_id_written=1
      else
        printf '%s\n' "$line"
      fi
    done < /etc/agent-i/agent.yaml > "$tmp_cfg"
    if [[ "$agent_id_written" -ne 1 ]]; then
      rm -f "$tmp_cfg"
      echo "error: no 'agent_id: \"\"' line in the shipped config to set" >&2
      exit 1
    fi
    # Overwrite in place so the mode and ownership set by install(1) above
    # survive; mv would replace them with mktemp's 0600 root-owned defaults.
    cat "$tmp_cfg" > /etc/agent-i/agent.yaml
    rm -f "$tmp_cfg"
    echo "    agent_id set to ${AGENT_I_AGENT_ID}"
  else
    echo "    agent_id left empty — the agent names itself after the host at startup"
  fi
else
  echo "    /etc/agent-i/agent.yaml already exists — leaving it in place"
fi

echo "==> preparing log/export directory"
mkdir -p /var/log/agent-i
chown agent-i:agent-i /var/log/agent-i

echo "==> preparing state directory"
# Holds the tailing offset registry, so a restart resumes where it left off
# instead of silently skipping whatever was logged while the agent was down.
mkdir -p /var/lib/agent-i
chown agent-i:agent-i /var/lib/agent-i
chmod 0750 /var/lib/agent-i

echo "==> preparing secrets file"
# Config never holds a credential value, only the NAME of the env var holding
# one — this is where those env vars actually get set. Root-owned and 0600:
# systemd reads it before dropping to the agent user, so the agent process
# never needs read access to the file itself.
if [[ ! -f /etc/agent-i/env ]]; then
  cat > /etc/agent-i/env <<'ENVEOF'
# Environment for agent-i. Values here are secrets; the agent.yaml
# config references them by variable name only.
#
# Examples — uncomment and fill in the ones you need:
# AGENT_I_API_TOKEN=            # if set, the dashboard API and OTLP receiver require
#                               # 'Authorization: Bearer <token>'. Unset means no auth.
# OTLP_INGESTION_KEY=
# AGENT_I_TRACE_TOKEN=
ENVEOF
  echo "    created /etc/agent-i/env — put ingestion keys there"
else
  echo "    /etc/agent-i/env already exists — leaving it in place"
fi
chown root:root /etc/agent-i/env
chmod 0600 /etc/agent-i/env

echo "==> installing systemd unit"
install -m 0644 systemd/agent-i.service /etc/systemd/system/agent-i.service
systemctl daemon-reload

echo "==> enabling and (re)starting service"
systemctl enable agent-i
# See get.sh for why this is a separate explicit restart rather than
# 'enable --now' — that command no-ops on an already-running service,
# which would leave a stale binary running after a rebuild-and-reinstall.
systemctl restart agent-i

echo "==> done. check status with: systemctl status agent-i"
echo "    tail output with:        journalctl -u agent-i -f"
