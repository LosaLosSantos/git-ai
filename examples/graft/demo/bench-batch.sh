#!/usr/bin/env bash
# Batching benchmark: two standalone Gitaly servers that are identical except
# for [graft.land.batch], the same graft-bench workload against each, and one
# combined Markdown table.
#
#   off  every UserLand request runs its own compare-and-swap loop
#   on   concurrent lands of one branch share one reference update (a batch)
#
# The workload is AGENT_COUNTS agents (20, 50 and 100 by default), CHANGES
# changes each (5), a conflict rate of RATE (0.2) and the change plan of SEED
# (1). Every run lands on a branch of its own, with rebase, on a repository of
# its server.
#
# Each server exposes its Prometheus metrics on 127.0.0.1 (METRICS_PORT_OFF and
# METRICS_PORT_ON). The metrics are scraped before and after every run, and the
# combined report ends with a "time per phase" table: the difference of the
# graft_land_batch_phase_seconds histograms of each run, per phase.
#
# Both servers and their repositories live under WORKDIR. The servers are
# stopped when the script exits, also on failure.
#
# Usage:
#   bash examples/graft/demo/bench-batch.sh [WORKDIR]
#
# Requires: make, go, openssl, curl and the Gitaly build dependencies (see
# examples/graft/quickstart.sh). WORKDIR defaults to a fresh temporary
# directory. Results (JSON, Markdown and Prometheus scrapes per run and the
# combined table) are in WORKDIR/results.
#
# Tunables (environment): AGENT_COUNTS ("20 50 100"), CHANGES (5), RATE (0.2),
# SEED (1), METRICS_PORT_OFF (9241), METRICS_PORT_ON (9242).

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
work="${1:-$(mktemp -d)}"
out="$work/results"
AGENT_COUNTS=${AGENT_COUNTS:-"20 50 100"}
CHANGES=${CHANGES:-5}
RATE=${RATE:-0.2}
SEED=${SEED:-1}
METRICS_PORT_OFF=${METRICS_PORT_OFF:-9241}
METRICS_PORT_ON=${METRICS_PORT_ON:-9242}
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

# metrics_port returns the metrics port of server NAME.
metrics_port() {
  if [ "$1" = on ]; then
    echo "$METRICS_PORT_ON"
  else
    echo "$METRICS_PORT_OFF"
  fi
}

# write_config writes the configuration of server NAME into WORKDIR/NAME. The
# two configurations differ only in the metrics port and the last section.
write_config() {
  local name="$1" disabled="$2" port="$3" dir="$work/$1"

  mkdir -p "$dir/repos" "$dir/run"
  {
    printf 'prometheus_listen_addr = "127.0.0.1:%s"\n' "$port"
    sed \
      -e "s|/var/lib/graft/bin|$work/bin|g" \
      -e "s|/var/lib/graft/run|$dir/run|g" \
      -e "s|/var/lib/graft/repos|$dir/repos|g" \
      -e "s|/etc/graft/tokens|$work/tokens|g" \
      -e "s|change-me-shared-token|$token|" \
      "$repo_root/examples/graft/config.toml"
  } > "$dir/config.toml"
  printf '\n[graft.land.batch]\ndisabled = %s\n' "$disabled" >> "$dir/config.toml"
}

log "Writing the configurations: batching off and batching on"
write_config off true "$METRICS_PORT_OFF"
write_config on false "$METRICS_PORT_ON"

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

wait_for_metrics() {
  local port="$1"
  for _ in $(seq 100); do
    curl -fsS "http://127.0.0.1:$port/metrics" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  echo "metrics on port $port did not appear" >&2
  return 1
}

start_server() {
  local name="$1"

  log "Starting Gitaly with batching $name"
  "$work/bin/gitaly" serve "$work/$name/config.toml" >"$out/gitaly-$name.log" 2>&1 &
  gitaly_pids+=("$!")
  wait_for_socket "$work/$name/run/gitaly.sock"
  wait_for_metrics "$(metrics_port "$name")"
}

start_server off
start_server on

# scrape writes the metrics of server NAME to FILE.
scrape() {
  local name="$1" file="$2"

  curl -fsS "http://127.0.0.1:$(metrics_port "$name")/metrics" >"$file"
}

# run_bench runs graft-bench against server NAME with AGENTS agents. The first
# run of a server creates its repository through the server's RepositoryService.
# The metrics are scraped before and after the run.
run_bench() {
  local name="$1" agents="$2" create=()

  if [ ! -e "$out/.created-$name" ]; then
    create=(--create-repo)
    : >"$out/.created-$name"
  fi

  scrape "$name" "$out/$name-$agents.before.prom"

  log "graft-bench against batching $name with $agents agents"
  "$work/bin/graft-bench" \
    --address "unix:$work/$name/run/gitaly.sock" \
    --storage default \
    --repo "$REPO" \
    "${create[@]}" \
    --branch "bench-$agents" \
    --agents "$agents" \
    --changes "$CHANGES" \
    --conflict-rate "$RATE" \
    --seed "$SEED" \
    --mode land \
    --token "$token" \
    --label "batching-$name-$agents" \
    --json-out "$out/$name-$agents.json" \
    --md-out "$out/$name-$agents.md"

  scrape "$name" "$out/$name-$agents.after.prom"
}

for agents in $AGENT_COUNTS; do # the counts are plain numbers
  for name in off on; do
    run_bench "$name" "$agents"
  done
done

files=()
for agents in $AGENT_COUNTS; do
  for name in off on; do
    files+=("$out/$name-$agents.json")
  done
done

log "Combined comparison"
{
  printf '## graft-bench: batched landing (%s changes per agent, conflict rate %s, seed %s)\n\n' "$CHANGES" "$RATE" "$SEED"
  "$work/bin/graft-bench" compare --format markdown "${files[@]}"
} | tee "$out/combined.md"

# phase_samples prints the sum and count of each phase of the histogram in FILE, one line per value:
# "LABEL sum|count PHASE VALUE".
phase_samples() {
  local file="$1" label="$2"

  sed -n 's/^graft_land_batch_phase_seconds_\(sum\|count\){phase="\([a-z_]*\)"} \(.*\)$/\1 \2 \3/p' "$file" |
    sed "s/^/$label /"
}

# phase_rows prints the rows of run RUN of the time per phase table: the difference of the scrapes
# BEFORE and AFTER the run.
phase_rows() {
  local run="$1" before="$2" after="$3"

  { phase_samples "$before" pre; phase_samples "$after" post; } | awk -v run="$run" '
    { v[$1, $2, $3] = $4 + 0 }
    END {
      batches = v["post", "count", "total"] - v["pre", "count", "total"]
      n = split("quarantine tip land_member plan replay pack update total", phases, " ")
      for (i = 1; i <= n; i++) {
        p = phases[i]
        count = v["post", "count", p] - v["pre", "count", p]
        sum = v["post", "sum", p] - v["pre", "sum", p]
        mean = count > 0 ? sum / count : 0
        printf "| %s | %s | %d | %.6f | %.6f |\n", run, p, count, sum, mean
        if (p == "land_member") {
          printf "| %s | land_member per batch | %d | %.6f | %.6f |\n", run, batches, sum, (batches > 0 ? sum / batches : 0)
        }
      }
    }'
}

log "Time per phase"
{
  printf '\n## Time per phase\n\n'
  printf 'The difference of the graft_land_batch_phase_seconds histograms between the scrapes before and after each run. '
  printf 'land_member is observed once per request of a batch attempt; "land_member per batch" is its sum divided by the number of batches. '
  printf 'Runs with batching off execute no batch, so their rows are zero.\n\n'
  printf '| run | phase | observations | sum (s) | mean (s) |\n|---|---|---:|---:|---:|\n'
  for agents in $AGENT_COUNTS; do
    for name in off on; do
      phase_rows "$name $agents agents" "$out/$name-$agents.before.prom" "$out/$name-$agents.after.prom"
    done
  done
} | tee -a "$out/combined.md"

echo
echo "bench-batch: results in $out"
