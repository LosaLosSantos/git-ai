#!/usr/bin/env bash
# Multi-node benchmark: the same graft-bench workload against NODES standalone
# Gitaly servers, with the repositories placed round robin over them, and
# against one server that holds the same repositories.
#
#   multi   NODES servers; repository r lives on node r modulo NODES, and the
#           agents work on their repository round robin, so every node serves
#           its share of the lands
#   single  one server with all REPOS repositories
#
# Every server has batching on ([graft.land.batch] disabled = false), its own
# repository storage and its own socket. The workload is AGENTS agents (60 by
# default) with CHANGES changes each (5), a conflict rate of RATE (0.2) and the
# change plan of SEED (1), landed with rebase. Both runs have the same workload.
#
# The combined report is a comparison table of the two aggregates, followed by
# the per-node results of the multi-node run.
#
# The servers are stopped when the script exits, also on failure.
#
# Usage:
#   bash examples/graft/demo/bench-multinode.sh [WORKDIR]
#
# Requires: make, go, openssl and the Gitaly build dependencies (see
# examples/graft/quickstart.sh). WORKDIR defaults to a fresh temporary
# directory and must be new, since the repositories are created. Results (JSON
# and Markdown per run, and the combined table) are in WORKDIR/results.
#
# Tunables (environment): AGENTS (60), CHANGES (5), REPOS (6), NODES (3),
# RATE (0.2), SEED (1).

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
work="${1:-$(mktemp -d)}"
out="$work/results"
AGENTS=${AGENTS:-60}
CHANGES=${CHANGES:-5}
REPOS=${REPOS:-6}
NODES=${NODES:-3}
RATE=${RATE:-0.2}
SEED=${SEED:-1}
REPO=bench.git

mkdir -p "$work/bin" "$work/tokens" "$out"

log() { printf '\n==> %s\n' "$*"; }

log "Building gitaly, the bundled Git, and graft-bench"
make -C "$repo_root" build
for binary in gitaly gitaly-hooks gitaly-ssh; do
  cp "$repo_root/_build/bin/$binary" "$work/bin/"
done
go build -C "$repo_root" -o "$work/bin/graft-bench" ./cmd/graft-bench

log "Writing tokens"
token="$(openssl rand -hex 32)"
for agent in planner builder; do
  openssl rand -hex 32 > "$work/tokens/$agent"
  chmod 0600 "$work/tokens/$agent"
done

# write_config writes the configuration of server NAME into WORKDIR/NAME. The
# server has its own repository storage and runtime directory, so the servers
# share nothing but the binaries.
write_config() {
  local name="$1" dir="$work/$1"

  mkdir -p "$dir/repos" "$dir/run"
  sed \
    -e "s|/var/lib/graft/bin|$work/bin|g" \
    -e "s|/var/lib/graft/run|$dir/run|g" \
    -e "s|/var/lib/graft/repos|$dir/repos|g" \
    -e "s|/etc/graft/tokens|$work/tokens|g" \
    -e "s|change-me-shared-token|$token|" \
    "$repo_root/examples/graft/config.toml" > "$dir/config.toml"
  printf '\n[graft.land.batch]\ndisabled = false\n' >> "$dir/config.toml"
}

gitaly_pids=()
cleanup() {
  for pid in "${gitaly_pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${gitaly_pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_for_socket() {
  local socket="$1"
  for _ in $(seq 100); do
    [ -S "$socket" ] && return 0
    sleep 0.1
  done
  echo "socket $socket did not appear" >&2
  return 1
}

# start_server starts server NAME and waits for its socket.
start_server() {
  local name="$1"

  log "Starting Gitaly $name"
  "$work/bin/gitaly" serve "$work/$name/config.toml" >"$out/gitaly-$name.log" 2>&1 &
  gitaly_pids+=("$!")
  wait_for_socket "$work/$name/run/gitaly.sock"
}

# The multi-node run: NODES servers, one address per server.
addrs=""
for n in $(seq "$NODES"); do
  write_config "node$n"
  start_server "node$n"
  addrs="${addrs:+$addrs,}unix:$work/node$n/run/gitaly.sock"
done

log "graft-bench across $NODES nodes: $REPOS repositories, $AGENTS agents"
"$work/bin/graft-bench" \
  --addrs "$addrs" \
  --storage default \
  --repo "$REPO" \
  --repos "$REPOS" \
  --create-repo \
  --branch bench-multi \
  --agents "$AGENTS" \
  --changes "$CHANGES" \
  --conflict-rate "$RATE" \
  --seed "$SEED" \
  --mode land \
  --token "$token" \
  --label "nodes-$NODES" \
  --json-out "$out/multi.json" \
  --md-out "$out/multi.md"

# The single-node run: the same repositories on one server.
write_config single
start_server single

log "graft-bench against one node: $REPOS repositories, $AGENTS agents"
"$work/bin/graft-bench" \
  --address "unix:$work/single/run/gitaly.sock" \
  --storage default \
  --repo "$REPO" \
  --repos "$REPOS" \
  --create-repo \
  --branch bench-single \
  --agents "$AGENTS" \
  --changes "$CHANGES" \
  --conflict-rate "$RATE" \
  --seed "$SEED" \
  --mode land \
  --token "$token" \
  --label "single-node" \
  --json-out "$out/single.json" \
  --md-out "$out/single.md"

log "Comparison"
{
  printf '## graft-bench: %s nodes against one node (%s agents, %s changes per agent, %s repositories, conflict rate %s, seed %s)\n\n' \
    "$NODES" "$AGENTS" "$CHANGES" "$REPOS" "$RATE" "$SEED"
  "$work/bin/graft-bench" compare --format markdown "$out/multi.json" "$out/single.json"
  awk '/^Per node/ { show = 1 } show' "$out/multi.md"
} | tee "$out/combined.md"

echo
echo "bench-multinode: results in $out"
