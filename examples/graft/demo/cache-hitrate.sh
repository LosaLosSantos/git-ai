#!/usr/bin/env bash
# Cache hit rate of a graft-cache daemon, without and with a seed.
#
# One standalone Gitaly serves a demo repository. Each phase starts a fresh
# graft-cache daemon tier in front of it and runs N sparse clones through the
# daemon. The cold phase starts the daemon empty. The seeded phase starts it
# with a seed file from "graft-cache snapshot" (the root tree of main, written
# before the phases), so that the clones are served from memory from the first
# one on. The table reports the hit rates from the daemon's /stats endpoint.
#
# Usage:
#   bash examples/graft/demo/cache-hitrate.sh [WORKDIR]
#
# Requires: make, go, openssl, git and curl, and the Gitaly build dependencies
# (see examples/graft/quickstart.sh). WORKDIR defaults to a fresh temporary
# directory. Results are in WORKDIR/results. The servers are stopped when the
# script exits, also on failure.
#
# Tunables (environment): N (10 clones per phase), METRICS_PORT (9237; the
# seeded phase uses the next port).

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
work="${1:-$(mktemp -d)}"
out="$work/results"
N=${N:-10}
METRICS_PORT=${METRICS_PORT:-9237}
REPO=demo.git

mkdir -p "$work"/{bin,repos,run,tokens,clones} "$out"
export GRAFT_STORAGE=default

log() { printf '\n==> %s\n' "$*"; }

log "Building gitaly, the bundled Git, graft and graft-cache (make build)"
make -C "$repo_root" build
for binary in gitaly gitaly-hooks gitaly-ssh graft graft-cache; do
  cp "$repo_root/_build/bin/$binary" "$work/bin/"
done

log "Writing agent tokens and the configuration"
for agent in planner builder; do
  openssl rand -hex 32 > "$work/tokens/$agent"
  chmod 0600 "$work/tokens/$agent"
done
sed \
  -e "s|/var/lib/graft/bin|$work/bin|g" \
  -e "s|/var/lib/graft/run|$work/run|g" \
  -e "s|/var/lib/graft/repos|$work/repos|g" \
  -e "s|/etc/graft/tokens|$work/tokens|g" \
  "$repo_root/examples/graft/config.toml" > "$work/config.toml"

pids=()
cleanup() {
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
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

wait_for_http() {
  local url="$1"
  for _ in $(seq 100); do
    curl -fsS "$url" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  echo "$url did not answer" >&2
  return 1
}

# as_agent runs a command with the token, identity and author of one agent.
as_agent() {
  local agent="$1"; shift
  GRAFT_TOKEN="$(cat "$work/tokens/$agent")" \
    GRAFT_USER="$agent" \
    GRAFT_AUTHOR="$agent <$agent@example.com>" \
    "$@"
}

log "Starting Gitaly in standalone mode"
"$work/bin/gitaly" serve "$work/config.toml" >"$out/gitaly.log" 2>&1 &
pids+=("$!")
wait_for_socket "$work/run/gitaly.sock"

log "Creating $REPO on main: four directories with 25 files each"
git init -q --bare "$work/repos/$REPO"
git init -q "$work/seed"
(
  cd "$work/seed"
  git symbolic-ref HEAD refs/heads/main
  for dir in src docs lib test; do
    mkdir -p "$dir/pkg"
    for i in $(seq 25); do
      printf '%s file %s\n' "$dir" "$i" > "$dir/pkg/file$i.txt"
    done
  done
  printf 'readme\n' > README.md
  git add -A
  git -c user.name=seed -c user.email=seed@example.com commit -qm "Seed"
  git push -q "$work/repos/$REPO" HEAD:refs/heads/main
)

log "Writing the seed: every tree and blob of the root tree of main"
"$work/bin/graft-cache" snapshot \
  --upstream "unix:$work/run/gitaly.sock" \
  --token "$(cat "$work/tokens/planner")" \
  --repo "$REPO" \
  --rev main \
  --out "$work/demo.seed"

# run_phase runs N sparse clones through a fresh daemon tier on port PORT and
# leaves its statistics in $out/stats-NAME.json. SEED, if set, is loaded at start.
run_phase() {
  local name="$1" port="$2" seed="${3:-}" address cache_pid
  local seed_args=()
  address="unix:$work/run/cache-$name.sock"
  if [ -n "$seed" ]; then
    seed_args=(--seed "$seed")
  fi

  log "Phase $name: daemon tier on port $port, seed: ${seed:-none}"
  "$work/bin/graft-cache" \
    --listen "$address" \
    --upstream "unix:$work/run/gitaly.sock" \
    --tier daemon \
    --max-bytes 268435456 \
    --metrics-addr "127.0.0.1:$port" \
    ${seed_args[@]+"${seed_args[@]}"} \
    >"$out/cache-$name.log" 2>&1 &
  cache_pid=$!
  pids+=("$cache_pid")
  wait_for_socket "$work/run/cache-$name.sock"
  wait_for_http "http://127.0.0.1:$port/stats"

  for i in $(seq "$N"); do
    as_agent planner "$work/bin/graft" \
      clone "$address" "$REPO" "$work/clones/$name-$i" --branch main --sparse src/,docs/ >/dev/null
  done

  curl -fsS "http://127.0.0.1:$port/stats" >"$out/stats-$name.json"
  kill "$cache_pid" 2>/dev/null || true
  wait "$cache_pid" 2>/dev/null || true
}

# stat_field prints the number of field NAME in the statistics file FILE.
stat_field() {
  local file="$1" name="$2"
  sed -n "s/.*\"$name\":\([0-9.eE+-]*\).*/\1/p" "$file"
}

run_phase cold "$METRICS_PORT"
run_phase seeded "$((METRICS_PORT + 1))" "$work/demo.seed"

log "Hit rates after $N sparse clones per phase"
{
  printf '| phase | clones | object hits | object misses | disk hits | seeded objects | object hit rate | hit rate |\n'
  printf '|---|---:|---:|---:|---:|---:|---:|---:|\n'
  for name in cold seeded; do
    file="$out/stats-$name.json"
    printf '| %s | %s | %s | %s | %s | %s | %s | %s |\n' \
      "$name" "$N" \
      "$(stat_field "$file" object_hits)" \
      "$(stat_field "$file" object_misses)" \
      "$(stat_field "$file" disk_hits)" \
      "$(stat_field "$file" seeded_objects)" \
      "$(stat_field "$file" object_hit_rate)" \
      "$(stat_field "$file" hit_rate)"
  done
} | tee "$out/hitrate.md"

echo
echo "cache-hitrate: results in $out"
