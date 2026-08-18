#!/usr/bin/env bash
# Agent-I one-line installer.
#
# Usage (once hosted — see HOSTING.md):
#   curl -fsSL https://<your-host>/get.sh | sudo bash
#
# Optional environment overrides:
#   AGENT_I_REPO=owner/repo                # GitHub repo to fetch releases from, if you forked
#   AGENT_I_VERSION=v1.2.0                 # defaults to the latest release
#
# What it does, in order:
#   1. detects OS + CPU architecture
#   2. downloads the matching prebuilt binary from GitHub Releases
#   3. verifies its SHA-256 checksum against the published checksums.txt
#   4. creates a dedicated, unprivileged 'agent-i' system user
#   5. installs the binary, default config, and systemd unit
#   6. enables + starts the service
#
# Deliberately does NOT require a Go toolchain on the target machine —
# that's the difference from scripts/install.sh (source build, for
# development) versus this one (binary install, for real deployments).
set -euo pipefail

# The repository is still named 'oneagent' even though the agent is not; the
# rename went as far as every identifier in the code and stopped at the GitHub
# repo itself. This has to match the repo that actually serves the releases,
# not the one the project is named after — pointing it at 'agent-i' made every
# download 404 with a message about the version being wrong. GitHub redirects
# renamed repos, so this keeps working if the repo is renamed later.
REPO="${AGENT_I_REPO:-Sannapanenitharun/oneagent}"
VERSION="${AGENT_I_VERSION:-latest}"

if [[ -z "${AGENT_I_REPO:-}" ]]; then
  echo "==> using default repo ${REPO} (override with AGENT_I_REPO=owner/repo)" >&2
fi

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (curl ... | sudo bash)" >&2
  exit 1
fi

# --- 1. detect platform ---
OS="$(uname -s)"
if [[ "$OS" != "Linux" ]]; then
  echo "error: Agent-I's host collector reads /proc for CPU/memory metrics, so only Linux is supported (detected: $OS)." >&2
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
if [[ -n "${AGENT_I_BASE_URL:-}" ]]; then
  # Explicit override — lets this installer work against non-GitHub
  # hosting (S3, your own server) or a local test server, bypassing the
  # GitHub Releases URL pattern entirely.
  BASE_URL="$AGENT_I_BASE_URL"
elif [[ "$VERSION" == "latest" ]]; then
  BASE_URL="https://github.com/${REPO}/releases/latest/download"
else
  BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
fi
TARBALL="agent-i_linux_${ARCH}.tar.gz"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

echo "==> downloading ${TARBALL} (${VERSION})"
curl -fsSL -o "$WORKDIR/$TARBALL" "$BASE_URL/$TARBALL" \
  || { echo "error: download failed — check AGENT_I_REPO=$REPO and AGENT_I_VERSION=$VERSION are correct" >&2; exit 1; }
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
EXTRACTED_DIR="$WORKDIR/agent-i_linux_${ARCH}"

# --- 5. system user ---
echo "==> creating system user (if absent)"
id -u agent-i >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --groups adm agent-i
# Idempotent: also fixes hosts that ran an older version of this script
# before 'adm' group membership was added — without it, the log tailer
# can't read most system logs (auth.log, syslog, etc. are typically
# root:adm 640) and silently collects nothing, which looks like "it's
# just not finding new lines" rather than the permission issue it is.
usermod -aG adm agent-i

# --- 6. install binary, config, systemd unit ---
echo "==> installing binary"
install -m 0755 "$EXTRACTED_DIR/agent-i" /usr/local/bin/agent-i

echo "==> installing auto-instrument helper"
install -m 0755 "$EXTRACTED_DIR/auto-instrument.sh" /usr/local/bin/agent-i-auto-instrument
echo "    run it any time with: sudo agent-i-auto-instrument        (dry run)"
echo "                          sudo agent-i-auto-instrument --apply (actually instruments + restarts detected services)"

echo "==> installing config"
mkdir -p /etc/agent-i
if [[ ! -f /etc/agent-i/agent.yaml ]]; then
  install -m 0640 -o agent-i -g agent-i "$EXTRACTED_DIR/agent.yaml" /etc/agent-i/agent.yaml
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
install -m 0644 "$EXTRACTED_DIR/agent-i.service" /etc/systemd/system/agent-i.service
systemctl daemon-reload

# --- 7. enable + (re)start ---
echo "==> enabling and (re)starting service"
systemctl enable agent-i
# 'enable --now' would silently no-op if the service is already running,
# leaving the OLD binary in memory even though we just wrote a new one to
# disk — explicitly restart so an update actually takes effect.
systemctl restart agent-i

echo "==> done. check status with: systemctl status agent-i"
echo "    tail output with:        journalctl -u agent-i -f"
