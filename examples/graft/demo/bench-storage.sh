#!/usr/bin/env bash
# Storage core benchmark: standalone Gitaly servers that are identical except
# for [graft.storage], the same graft-bench workload against each, and one
# combined Markdown table.
#
#   off       the storage core is off (the baseline: local git only)
#   sqlite    authoritative mode, SQLite database and a file bucket
#   postgres  authoritative mode, Postgres database and a file bucket
#
# The workload lands WRITERS concurrent writers (1, 10 and 50 by default),
# CHANGES changes each (5), with a conflict rate of RATE (0) and the change plan
# of SEED (1), on a branch of its own per run, with rebase, on one repository per
# server. Batched landing is configured as in examples/graft/config.toml (on).
#
# Each server exposes its Prometheus metrics on 127.0.0.1 (METRICS_PORT_BASE,
# +1, +2). The metrics are scraped before and after every run. The report has
# two tables: the graft-bench comparison (lands per minute, latency, attempts)
# and the storage core overhead per run: the difference of the
# graft_storage_preparing_seconds histogram (the voter in the Preparing phase,
# before git locks the references: pack build and upload), of the
# graft_storage_prepared_seconds histogram (the voter in the Prepared phase,
# while git holds the reference locks: expected values and database commit) and
# of the graft_storage_sync_seconds histogram (EnsureFresh before every request).
#
# The postgres server needs POSTGRES_URL: the URL of a Postgres server on which
# the script may create a database, for example
# postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable. It defaults to
# GRAFT_TEST_POSTGRES_DSN. The script creates a fresh database graft_bench_<hex>
# and drops it when it exits. Without a URL the postgres server is skipped.
# Credentials come from the PG* environment variables or .pgpass.
#
# Every server and its repositories, database (SQLite) and bucket live under
# WORKDIR. The servers are stopped when the script exits, also on failure.
#
# Usage:
#   bash examples/graft/demo/bench-storage.sh [WORKDIR]
#
# Requires: make, go, openssl, curl and the Gitaly build dependencies (see
# examples/graft/quickstart.sh). WORKDIR defaults to a fresh temporary
# directory. Results (JSON, Markdown and Prometheus scrapes per run and the
# combined report) are in WORKDIR/results.
#
# Tunables (environment): BACKENDS ("off sqlite postgres"), WRITERS
# ("1 10 50"), CHANGES (5), RATE (0), SEED (1), METRICS_PORT_BASE (9251),
# POSTGRES_URL, MAKE_ARGS (extra arguments of `make build`, for example
# DEPENDENCY_DIR=... to reuse an existing build of Git).

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
work="${1:-$(mktemp -d)}"
out="$work/results"
BACKENDS=${BACKENDS:-"off sqlite postgres"}
WRITERS=${WRITERS:-"1 10 50"}
CHANGES=${CHANGES:-5}
RATE=${RATE:-0}
SEED=${SEED:-1}
METRICS_PORT_BASE=${METRICS_PORT_BASE:-9251}
POSTGRES_URL=${POSTGRES_URL:-${GRAFT_TEST_POSTGRES_DSN:-}}
MAKE_ARGS=${MAKE_ARGS:-}
REPO=bench.git

mkdir -p "$work/bin" "$work/tokens" "$work/pgdb" "$out"

log() { printf '\n==> %s\n' "$*"; }

log "Building gitaly, the bundled Git, and graft-bench"
# MAKE_ARGS is split into words on purpose: it holds make arguments.
# shellcheck disable=SC2086
make -C "$repo_root" build $MAKE_ARGS
for binary in gitaly gitaly-hooks gitaly-ssh; do
  cp "$repo_root/_build/bin/$binary" "$work/bin/"
done
go build -C "$repo_root" -o "$work/bin/graft-bench" ./cmd/graft-bench

# graft-pgdb creates and drops the benchmark's Postgres database with the
# driver the server uses, so that the script needs no psql.
cat >"$work/pgdb/main.go" <<'EOF'
// Command graft-pgdb creates ("create ADMIN_URL", prints the new database's
// URL) or drops ("drop ADMIN_URL URL") a Postgres database of the storage
// benchmark.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "graft-pgdb:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	const usage = "usage: graft-pgdb create ADMIN_URL | drop ADMIN_URL URL"
	if len(args) < 2 {
		return errors.New(usage)
	}
	u, err := url.Parse(args[1])
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch args[0] {
	case "create":
		if len(args) != 2 {
			return errors.New(usage)
		}
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			return err
		}
		name := "graft_bench_" + hex.EncodeToString(suffix)
		if err := exec(ctx, u.String(), "CREATE DATABASE "+name); err != nil {
			return err
		}
		u.Path = "/" + name
		fmt.Println(u.String())
		return nil
	case "drop":
		if len(args) != 3 {
			return errors.New(usage)
		}
		target, err := url.Parse(args[2])
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(target.Path, "/")
		// Only a name that "create" can return is dropped: it is concatenated
		// into the statement.
		if !regexp.MustCompile(`^graft_bench_[0-9a-f]{12}$`).MatchString(name) {
			return fmt.Errorf("refusing to drop %q: not a benchmark database", name)
		}
		return exec(ctx, u.String(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func exec(ctx context.Context, dsn, statement string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, statement)
	return err
}
EOF
go build -C "$repo_root" -o "$work/bin/graft-pgdb" "$work/pgdb/main.go"

log "Writing tokens"
token="$(openssl rand -hex 32)"
for agent in planner builder; do
  openssl rand -hex 32 > "$work/tokens/$agent"
  chmod 0600 "$work/tokens/$agent"
done

pg_database=""
backends=()
for backend in $BACKENDS; do # the names are plain words
  if [ "$backend" = postgres ]; then
    if [ -z "$POSTGRES_URL" ]; then
      log "POSTGRES_URL is not set: skipping the postgres server"
      continue
    fi
    pg_database="$("$work/bin/graft-pgdb" create "$POSTGRES_URL")"
  fi
  backends+=("$backend")
done

# metrics_port returns the metrics port of server NAME.
metrics_port() {
  case "$1" in
    off) echo "$METRICS_PORT_BASE" ;;
    sqlite) echo $((METRICS_PORT_BASE + 1)) ;;
    postgres) echo $((METRICS_PORT_BASE + 2)) ;;
  esac
}

# write_config writes the configuration of server NAME into WORKDIR/NAME. The
# configurations differ only in the metrics port and [graft.storage].
write_config() {
  local name="$1" dir="$work/$1"

  mkdir -p "$dir/repos" "$dir/run"
  {
    printf 'prometheus_listen_addr = "127.0.0.1:%s"\n' "$(metrics_port "$name")"
    sed \
      -e "s|/var/lib/graft/bin|$work/bin|g" \
      -e "s|/var/lib/graft/run|$dir/run|g" \
      -e "s|/var/lib/graft/repos|$dir/repos|g" \
      -e "s|/etc/graft/tokens|$work/tokens|g" \
      -e "s|change-me-shared-token|$token|" \
      "$repo_root/examples/graft/config.toml"
  } > "$dir/config.toml"

  local database=""
  case "$name" in
    off) return 0 ;;
    sqlite) database="sqlite://$dir/meta.db" ;;
    postgres) database="$pg_database" ;;
  esac
  cat >>"$dir/config.toml" <<EOF

[graft.storage]
mode = "authoritative"
database = "$database"
bucket = "file://$dir/objects"
node_id = "bench-$name"
EOF
}

gitaly_pids=()
cleanup() {
  for pid in "${gitaly_pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${gitaly_pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  if [ -n "$pg_database" ]; then
    "$work/bin/graft-pgdb" drop "$POSTGRES_URL" "$pg_database" || true
  fi
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

for name in "${backends[@]}"; do
  log "Starting Gitaly with the storage core: $name"
  write_config "$name"
  "$work/bin/gitaly" serve "$work/$name/config.toml" >"$out/gitaly-$name.log" 2>&1 &
  gitaly_pids+=("$!")
  wait_for_socket "$work/$name/run/gitaly.sock"
  wait_for_metrics "$(metrics_port "$name")"
done

# scrape writes the metrics of server NAME to FILE.
scrape() {
  curl -fsS "http://127.0.0.1:$(metrics_port "$1")/metrics" >"$2"
}

# run_bench runs graft-bench against server NAME with WRITERS writers. The
# first run of a server creates its repository through the server's
# RepositoryService (in authoritative mode the storage core registers it).
run_bench() {
  local name="$1" writers="$2" create=()

  if [ ! -e "$out/.created-$name" ]; then
    create=(--create-repo)
    : >"$out/.created-$name"
  fi

  scrape "$name" "$out/$name-$writers.before.prom"

  log "graft-bench against $name with $writers writers"
  "$work/bin/graft-bench" \
    --address "unix:$work/$name/run/gitaly.sock" \
    --storage default \
    --repo "$REPO" \
    "${create[@]}" \
    --branch "bench-$writers" \
    --agents "$writers" \
    --changes "$CHANGES" \
    --conflict-rate "$RATE" \
    --seed "$SEED" \
    --mode land \
    --token "$token" \
    --label "$name-$writers" \
    --json-out "$out/$name-$writers.json" \
    --md-out "$out/$name-$writers.md"

  scrape "$name" "$out/$name-$writers.after.prom"
}

for writers in $WRITERS; do # the counts are plain numbers
  for name in "${backends[@]}"; do
    run_bench "$name" "$writers"
  done
done

files=()
for writers in $WRITERS; do
  for name in "${backends[@]}"; do
    files+=("$out/$name-$writers.json")
  done
done

log "Combined comparison"
{
  printf '## graft-bench: storage core (%s changes per writer, conflict rate %s, seed %s)\n\n' "$CHANGES" "$RATE" "$SEED"
  "$work/bin/graft-bench" compare --format markdown "${files[@]}"
} | tee "$out/combined.md"

# json_number prints the number of KEY in the graft-bench JSON FILE.
json_number() {
  sed -n "s/^  \"$2\": \([-0-9.e+]*\),\{0,1\}$/\1/p" "$1" | head -1
}

# histogram_samples prints "LABEL sum|count NAME VALUE" for the sum and count
# of the histograms of the storage core in the scrape FILE.
histogram_samples() {
  sed -n 's/^\(graft_storage_preparing_seconds\|graft_storage_prepared_seconds\|graft_storage_sync_seconds\)_\(sum\|count\) \(.*\)$/\2 \1 \3/p' "$1" |
    sed "s/^/$2 /"
}

# overhead_row prints the row of run RUN of the overhead table: the difference
# of the scrapes BEFORE and AFTER, and the throughput of the JSON result.
overhead_row() {
  local run="$1" before="$2" after="$3" json="$4"

  { histogram_samples "$before" pre; histogram_samples "$after" post; } | awk \
    -v run="$run" -v lpm="$(json_number "$json" lands_per_minute)" \
    -v p50="$(json_number "$json" p50_ms)" -v p95="$(json_number "$json" p95_ms)" '
    { v[$1, $2, $3] = $4 + 0 }
    END {
      rc = v["post", "count", "graft_storage_preparing_seconds"] - v["pre", "count", "graft_storage_preparing_seconds"]
      rs = v["post", "sum", "graft_storage_preparing_seconds"] - v["pre", "sum", "graft_storage_preparing_seconds"]
      pc = v["post", "count", "graft_storage_prepared_seconds"] - v["pre", "count", "graft_storage_prepared_seconds"]
      ps = v["post", "sum", "graft_storage_prepared_seconds"] - v["pre", "sum", "graft_storage_prepared_seconds"]
      sc = v["post", "count", "graft_storage_sync_seconds"] - v["pre", "count", "graft_storage_sync_seconds"]
      ss = v["post", "sum", "graft_storage_sync_seconds"] - v["pre", "sum", "graft_storage_sync_seconds"]
      printf "| %s | %.1f | %.1f | %.1f | %d | %d | %.2f | %.2f | %d | %.2f |\n", run, lpm, p50, p95,
        pc, rc, (rc > 0 ? 1000 * rs / rc : 0), (pc > 0 ? 1000 * ps / pc : 0), sc, (sc > 0 ? 1000 * ss / sc : 0)
    }'
}

log "Storage core overhead"
{
  printf '\n## Storage core overhead\n\n'
  printf 'Throughput and latency of the landed changes (graft-bench), and the difference of the storage core histograms between the scrapes before and after each run. '
  printf 'Preparing: graft_storage_preparing_seconds, the voter before git locks the references (pack build and upload). '
  printf 'Locked: graft_storage_prepared_seconds, once per reference transaction while git holds the reference locks (expected values and database commit; also the pack when the Preparing phase did not prepare it). '
  printf 'Sync: graft_storage_sync_seconds, once per EnsureFresh before a request. The off server has none of them.\n\n'
  printf '| run | lands/min | p50 (ms) | p95 (ms) | voter transactions | preparing calls | preparing mean (ms) | locked mean (ms) | syncs | sync mean (ms) |\n'
  printf '|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n'
  for writers in $WRITERS; do
    for name in "${backends[@]}"; do
      overhead_row "$name $writers writers" "$out/$name-$writers.before.prom" "$out/$name-$writers.after.prom" "$out/$name-$writers.json"
    done
  done
} | tee -a "$out/combined.md"

echo
echo "bench-storage: results in $out"
