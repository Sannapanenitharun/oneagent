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

echo "==> installing config"
mkdir -p /etc/agent-i
if [[ ! -f /etc/agent-i/agent.yaml ]]; then
  install -m 0640 -o agent-i -g agent-i configs/agent.yaml /etc/agent-i/agent.yaml
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
# OTLP_INGESTION_KEY=
# AGENT_I_TRACE_TOKEN=
# AWS_ACCESS_KEY_ID=
# AWS_SECRET_ACCESS_KEY=
# AWS_SESSION_TOKEN=
ENVEOF
  echo "    created /etc/agent-i/env — put ingestion keys and cloud credentials there"
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
