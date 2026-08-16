#!/usr/bin/env bash
# Forwards a remote agent's dashboard port to this machine, and keeps it up.
#
# The agent's dashboard binds loopback on purpose: it has no authentication and
# serves that host's metrics, logs and trace contents. An SSH forward is how you
# reach it from a browser without publishing any of that — do NOT move
# dashboard.listen_addr off 127.0.0.1 to avoid needing this.
#
# Usage:
#   scripts/dev-tunnel.sh user@host
#   scripts/dev-tunnel.sh -i ~/.ssh/key.pem user@host
#   LOCAL_PORT=9000 scripts/dev-tunnel.sh user@host
#
# Then point the frontend at it:
#   AGENT_I_URL=http://127.0.0.1:8089 npm run dev
#
# Why the reconnect loop: a plain `ssh -N` forward has no keepalive, so an idle
# connection gets dropped by the network or by sshd and simply stays dead. The
# dashboard then shows "agent not reachable" until a human notices. This
# reconnects instead.
set -uo pipefail

LOCAL_PORT="${LOCAL_PORT:-8089}"
REMOTE_PORT="${REMOTE_PORT:-8088}"
IDENTITY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -i) IDENTITY="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) TARGET="$1"; shift ;;
  esac
done

if [[ -z "${TARGET:-}" ]]; then
  echo "usage: $0 [-i identity_file] user@host" >&2
  echo "       LOCAL_PORT=$LOCAL_PORT REMOTE_PORT=$REMOTE_PORT are overridable" >&2
  exit 1
fi

SSH_ARGS=(-o BatchMode=yes -o ExitOnForwardFailure=yes
          -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o TCPKeepAlive=yes
          -N -L "${LOCAL_PORT}:127.0.0.1:${REMOTE_PORT}")
[[ -n "$IDENTITY" ]] && SSH_ARGS=(-i "$IDENTITY" "${SSH_ARGS[@]}")

echo "forwarding 127.0.0.1:${LOCAL_PORT} -> ${TARGET} 127.0.0.1:${REMOTE_PORT}"
echo "point the UI at it with: AGENT_I_URL=http://127.0.0.1:${LOCAL_PORT} npm run dev"

fails=0
while true; do
  ssh "${SSH_ARGS[@]}" "$TARGET"
  code=$?
  # Ctrl-C should stop the loop, not restart the connection.
  [[ $code -eq 130 ]] && exit 0
  # 255 is ssh's own error: auth, DNS, a port already bound. Those do not fix
  # themselves, so back off hard rather than hammering the host — three fast
  # retries covers a genuine transient, and anything past that is a real fault.
  if [[ $code -eq 255 ]]; then
    fails=$((fails + 1))
    if [[ $fails -ge 3 ]]; then
      echo "ssh failed ${fails} times (exit 255) — check the identity file, the host, and whether ${LOCAL_PORT} is already in use" >&2
      sleep 30
    else
      sleep 5
    fi
  else
    fails=0
    echo "connection dropped (exit ${code}) — reconnecting"
    sleep 3
  fi
done
