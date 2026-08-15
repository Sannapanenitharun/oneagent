#!/usr/bin/env bash
# OneAgent Agent installer.
# Usage (from within the oneagent-agent repo/tarball root):
#   sudo ./scripts/install.sh
#
# What it does, in order:
#   1. builds the static Go binary
#   2. creates a dedicated, unprivileged 'oneagent-agent' system user
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

echo "==> building oneagent-agent"
# Stamp the build with the checkout it came from, so `oneagent-agent
# --version` and the telemetry.distro.version resource attribute both
# identify exactly what is deployed. A "-dirty" suffix means the working
# tree had uncommitted changes at build time — that binary corresponds to
# no commit, which is worth knowing when a host reports something the
# committed code cannot produce.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
echo "    version: ${VERSION}"
CGO_ENABLED=0 go build \
  -ldflags "-X github.com/oneagent/agent/internal/version.Version=${VERSION}" \
  -o /tmp/oneagent-agent ./cmd/agent

echo "==> installing binary"
install -m 0755 /tmp/oneagent-agent /usr/local/bin/oneagent-agent

echo "==> installing auto-instrument helper"
install -m 0755 scripts/auto-instrument.sh /usr/local/bin/oneagent-auto-instrument

echo "==> creating system user (if absent)"
id -u oneagent-agent >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --groups adm oneagent-agent
# See get.sh for why this is unconditional (idempotent) rather than only
# on first creation.
usermod -aG adm oneagent-agent

echo "==> installing config"
mkdir -p /etc/oneagent-agent
if [[ ! -f /etc/oneagent-agent/agent.yaml ]]; then
  install -m 0640 -o oneagent-agent -g oneagent-agent configs/agent.yaml /etc/oneagent-agent/agent.yaml
else
  echo "    /etc/oneagent-agent/agent.yaml already exists — leaving it in place"
fi

echo "==> preparing log/export directory"
mkdir -p /var/log/oneagent-agent
chown oneagent-agent:oneagent-agent /var/log/oneagent-agent

echo "==> preparing state directory"
# Holds the tailing offset registry, so a restart resumes where it left off
# instead of silently skipping whatever was logged while the agent was down.
mkdir -p /var/lib/oneagent-agent
chown oneagent-agent:oneagent-agent /var/lib/oneagent-agent
chmod 0750 /var/lib/oneagent-agent

echo "==> preparing secrets file"
# Config never holds a credential value, only the NAME of the env var holding
# one — this is where those env vars actually get set. Root-owned and 0600:
# systemd reads it before dropping to the agent user, so the agent process
# never needs read access to the file itself.
if [[ ! -f /etc/oneagent-agent/env ]]; then
  cat > /etc/oneagent-agent/env <<'ENVEOF'
# Environment for oneagent-agent. Values here are secrets; the agent.yaml
# config references them by variable name only.
#
# Examples — uncomment and fill in the ones you need:
# SIGNOZ_INGESTION_KEY=
# ONEAGENT_TRACE_TOKEN=
# AWS_ACCESS_KEY_ID=
# AWS_SECRET_ACCESS_KEY=
# AWS_SESSION_TOKEN=
ENVEOF
  echo "    created /etc/oneagent-agent/env — put ingestion keys and cloud credentials there"
else
  echo "    /etc/oneagent-agent/env already exists — leaving it in place"
fi
chown root:root /etc/oneagent-agent/env
chmod 0600 /etc/oneagent-agent/env

echo "==> installing systemd unit"
install -m 0644 systemd/oneagent-agent.service /etc/systemd/system/oneagent-agent.service
systemctl daemon-reload

echo "==> enabling and (re)starting service"
systemctl enable oneagent-agent
# See get.sh for why this is a separate explicit restart rather than
# 'enable --now' — that command no-ops on an already-running service,
# which would leave a stale binary running after a rebuild-and-reinstall.
systemctl restart oneagent-agent

echo "==> done. check status with: systemctl status oneagent-agent"
echo "    tail output with:        journalctl -u oneagent-agent -f"
