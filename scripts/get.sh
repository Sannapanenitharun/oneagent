#!/usr/bin/env bash
# OneAgent one-line installer.
#
# Usage (once hosted — see HOSTING.md):
#   curl -fsSL https://<your-host>/get.sh | sudo bash
#
# Optional environment overrides:
#   ONEAGENT_REPO=Sannapanenitharun/oneagent   # GitHub "owner/repo" to fetch releases from
#   ONEAGENT_VERSION=v1.2.0                 # defaults to the latest release
#
# What it does, in order:
#   1. detects OS + CPU architecture
#   2. downloads the matching prebuilt binary from GitHub Releases
#   3. verifies its SHA-256 checksum against the published checksums.txt
#   4. creates a dedicated, unprivileged 'oneagent-agent' system user
#   5. installs the binary, default config, and systemd unit
#   6. enables + starts the service
#
# Deliberately does NOT require a Go toolchain on the target machine —
# that's the difference from scripts/install.sh (source build, for
# development) versus this one (binary install, for real deployments).
set -euo pipefail

REPO="${ONEAGENT_REPO:-Sannapanenitharun/oneagent}"
VERSION="${ONEAGENT_VERSION:-latest}"

if [[ "$REPO" == "Sannapanenitharun/oneagent" ]]; then
  echo "==> using default repo Sannapanenitharun/oneagent (override with ONEAGENT_REPO=owner/repo)" >&2
fi

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (curl ... | sudo bash)" >&2
  exit 1
fi

# --- 1. detect platform ---
OS="$(uname -s)"
if [[ "$OS" != "Linux" ]]; then
  echo "error: OneAgent's host collector reads /proc for CPU/memory metrics, so only Linux is supported (detected: $OS)." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "error: unsupported architecture $(uname -m) — only amd64 and arm64 have published builds." >&2
    exit 1
    ;;
esac
echo "==> detected linux/${ARCH}"

# --- 2. resolve download URLs ---
if [[ -n "${ONEAGENT_BASE_URL:-}" ]]; then
  # Explicit override — lets this installer work against non-GitHub
  # hosting (S3, your own server) or a local test server, bypassing the
  # GitHub Releases URL pattern entirely.
  BASE_URL="$ONEAGENT_BASE_URL"
elif [[ "$VERSION" == "latest" ]]; then
  BASE_URL="https://github.com/${REPO}/releases/latest/download"
else
  BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
fi
TARBALL="oneagent-agent_linux_${ARCH}.tar.gz"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

echo "==> downloading ${TARBALL} (${VERSION})"
curl -fsSL -o "$WORKDIR/$TARBALL" "$BASE_URL/$TARBALL" \
  || { echo "error: download failed — check ONEAGENT_REPO=$REPO and ONEAGENT_VERSION=$VERSION are correct" >&2; exit 1; }
curl -fsSL -o "$WORKDIR/checksums.txt" "$BASE_URL/checksums.txt" \
  || { echo "error: could not download checksums.txt — refusing to install an unverified binary" >&2; exit 1; }

# --- 3. verify checksum before running anything from the archive ---
echo "==> verifying checksum"
EXPECTED="$(grep "  ${TARBALL}\$" "$WORKDIR/checksums.txt" | awk '{print $1}')"
if [[ -z "$EXPECTED" ]]; then
  echo "error: no checksum entry found for ${TARBALL} in checksums.txt" >&2
  exit 1
fi
ACTUAL="$(sha256sum "$WORKDIR/$TARBALL" | awk '{print $1}')"
if [[ "$EXPECTED" != "$ACTUAL" ]]; then
  echo "error: checksum mismatch for ${TARBALL}" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  actual:   $ACTUAL" >&2
  exit 1
fi
echo "    checksum OK"

# --- 4. extract ---
tar -xzf "$WORKDIR/$TARBALL" -C "$WORKDIR"
EXTRACTED_DIR="$WORKDIR/oneagent-agent_linux_${ARCH}"

# --- 5. system user ---
echo "==> creating system user (if absent)"
id -u oneagent-agent >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --groups adm oneagent-agent
# Idempotent: also fixes hosts that ran an older version of this script
# before 'adm' group membership was added — without it, the log tailer
# can't read most system logs (auth.log, syslog, etc. are typically
# root:adm 640) and silently collects nothing, which looks like "it's
# just not finding new lines" rather than the permission issue it is.
usermod -aG adm oneagent-agent

# --- 6. install binary, config, systemd unit ---
echo "==> installing binary"
install -m 0755 "$EXTRACTED_DIR/oneagent-agent" /usr/local/bin/oneagent-agent

echo "==> installing auto-instrument helper"
install -m 0755 "$EXTRACTED_DIR/auto-instrument.sh" /usr/local/bin/oneagent-auto-instrument
echo "    run it any time with: sudo oneagent-auto-instrument        (dry run)"
echo "                          sudo oneagent-auto-instrument --apply (actually instruments + restarts detected services)"

echo "==> installing config"
mkdir -p /etc/oneagent-agent
if [[ ! -f /etc/oneagent-agent/agent.yaml ]]; then
  install -m 0640 -o oneagent-agent -g oneagent-agent "$EXTRACTED_DIR/agent.yaml" /etc/oneagent-agent/agent.yaml
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
install -m 0644 "$EXTRACTED_DIR/oneagent-agent.service" /etc/systemd/system/oneagent-agent.service
systemctl daemon-reload

# --- 7. enable + (re)start ---
echo "==> enabling and (re)starting service"
systemctl enable oneagent-agent
# 'enable --now' would silently no-op if the service is already running,
# leaving the OLD binary in memory even though we just wrote a new one to
# disk — explicitly restart so an update actually takes effect.
systemctl restart oneagent-agent

echo "==> done. check status with: systemctl status oneagent-agent"
echo "    tail output with:        journalctl -u oneagent-agent -f"
