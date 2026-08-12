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
CGO_ENABLED=0 go build -o /tmp/oneagent-agent ./cmd/agent

echo "==> installing binary"
install -m 0755 /tmp/oneagent-agent /usr/local/bin/oneagent-agent

echo "==> creating system user (if absent)"
id -u oneagent-agent >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin oneagent-agent

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

echo "==> installing systemd unit"
install -m 0644 systemd/oneagent-agent.service /etc/systemd/system/oneagent-agent.service
systemctl daemon-reload

echo "==> enabling and starting service"
systemctl enable --now oneagent-agent

echo "==> done. check status with: systemctl status oneagent-agent"
echo "    tail output with:        journalctl -u oneagent-agent -f"
