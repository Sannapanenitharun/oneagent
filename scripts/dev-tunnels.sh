#!/usr/bin/env bash
# Forwards MANY agents' dashboard ports to this machine at once, and keeps them
# up.
#
# dev-tunnel.sh does one host. That is the right tool for one server and the
# wrong one for ten across different accounts, where the work is not the ssh
# invocation but keeping track of which local port belongs to which machine and
# restarting whichever one died.
#
# The agent's dashboard binds loopback on purpose: it has no authentication and
# serves that host's metrics, logs and trace contents. An SSH forward is how you
# reach it from a browser without publishing any of that — do NOT move
# dashboard.listen_addr off 127.0.0.1 to avoid needing this.
#
# Usage:
#   scripts/dev-tunnels.sh hosts.txt
#   scripts/dev-tunnels.sh -i ~/.ssh/key.pem hosts.txt
#
# hosts.txt is one server per line. Blank lines and # comments are ignored:
#
#   # name          ssh target                  [identity]
#   prod-web-1      ec2-user@54.209.31.122      ~/.ssh/prod.pem
#   prod-web-2      ec2-user@54.210.11.9        ~/.ssh/prod.pem
#   staging-api     ubuntu@18.201.4.7           ~/.ssh/staging.pem
#
# The identity column is optional and is what makes several AWS accounts work
# in one file — each host can carry its own key. -i supplies a default for any
# line that omits it.
#
# Local ports are assigned in order from BASE_PORT (8089 by default), so the
# first host is 8089, the second 8090, and so on. On exit it prints the list in
# the format the dashboard's host manager accepts, so wiring the UI up is a
# copy and paste rather than a counting exercise.
set -uo pipefail

BASE_PORT="${BASE_PORT:-8089}"
REMOTE_PORT="${REMOTE_PORT:-8088}"
DEFAULT_IDENTITY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -i) DEFAULT_IDENTITY="$2"; shift 2 ;;
    -h|--help) sed -n '2,32p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) HOSTS_FILE="$1"; shift ;;
  esac
done

if [[ -z "${HOSTS_FILE:-}" ]]; then
  echo "usage: $0 [-i default_identity] hosts.txt" >&2
  echo "       BASE_PORT=$BASE_PORT REMOTE_PORT=$REMOTE_PORT are overridable" >&2
  exit 1
fi
if [[ ! -r "$HOSTS_FILE" ]]; then
  echo "cannot read $HOSTS_FILE" >&2
  exit 1
fi

# Children are killed on exit rather than left behind. Without this, a second
# run fails on every port with "address already in use" and the cause — orphans
# from the previous run — is not visible from the error.
PIDS=()
cleanup() {
  trap - EXIT INT TERM
  for pid in "${PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null
  done
  wait 2>/dev/null
  exit 0
}
trap cleanup EXIT INT TERM

# forward keeps one host's tunnel alive. Same reconnect reasoning as
# dev-tunnel.sh: a plain `ssh -N` forward has no keepalive, so an idle
# connection is dropped by the network or by sshd and simply stays dead.
forward() {
  local name="$1" target="$2" identity="$3" local_port="$4"
  local args=(-o BatchMode=yes -o ExitOnForwardFailure=yes
              -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o TCPKeepAlive=yes
              -N -L "127.0.0.1:${local_port}:127.0.0.1:${REMOTE_PORT}")
  [[ -n "$identity" ]] && args=(-i "${identity/#\~/$HOME}" "${args[@]}")

  local fails=0
  while true; do
    ssh "${args[@]}" "$target"
    local code=$?
    [[ $code -eq 130 ]] && return 0
    # 255 is ssh's own error: auth, DNS, a port already bound. Those do not fix
    # themselves, so back off hard rather than hammering the host. Reported per
    # host and non-fatal — one unreachable server in a fleet of ten must not
    # take down the nine tunnels that are working.
    if [[ $code -eq 255 ]]; then
      fails=$((fails + 1))
      if [[ $fails -ge 3 ]]; then
        echo "[$name] ssh failed $fails times (exit 255) — check the key, the target and whether ${local_port} is free; retrying in 60s" >&2
        sleep 60
        fails=0
        continue
      fi
      sleep 5
    else
      fails=0
      echo "[$name] tunnel dropped (exit $code) — reconnecting" >&2
      sleep 2
    fi
  done
}

port=$BASE_PORT
SPEC=()
COUNT=0

while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line%%#*}"                       # strip comments
  line="$(echo "$line" | xargs)"           # collapse whitespace, trim
  [[ -z "$line" ]] && continue

  # shellcheck disable=SC2206 # deliberate word splitting: the format is columns
  cols=($line)
  name="${cols[0]:-}"
  target="${cols[1]:-}"
  identity="${cols[2]:-$DEFAULT_IDENTITY}"

  if [[ -z "$target" ]]; then
    echo "skipping malformed line (need: name user@host [identity]): $line" >&2
    continue
  fi

  echo "[$name] 127.0.0.1:${port} -> ${target} 127.0.0.1:${REMOTE_PORT}"
  forward "$name" "$target" "$identity" "$port" &
  PIDS+=($!)
  SPEC+=("${name}=http://127.0.0.1:${port}")
  COUNT=$((COUNT + 1))
  port=$((port + 1))
done < "$HOSTS_FILE"

if [[ $COUNT -eq 0 ]]; then
  echo "no usable hosts in $HOSTS_FILE" >&2
  exit 1
fi

printf '\n%d tunnel(s) up. Paste this into the dashboard host manager:\n\n' "$COUNT"
printf '%s\n' "${SPEC[@]}"
printf '\nOr seed it from the shell instead:\n\n  AGENT_I_HOSTS="%s" npm run dev\n\n' "$(IFS=,; echo "${SPEC[*]}")"
echo "Ctrl-C to bring them all down."

wait
