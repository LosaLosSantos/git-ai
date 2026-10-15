#!/bin/sh
# Demo: lands the same workload through three modes and prints a comparison
# table. Each mode uses graft-bench with 50 agents (by default), landing
# changes on one branch of a running Graft server.
#
#   land          UserLand with rebase. Conflicting changes fail.
#   land-resolve  UserLand with automatic conflict resolution (graft-resolve).
#   queue         Serialized merge queue: one change at a time, each followed
#                 by a simulated test of TEST_MS milliseconds.
#
# Setup, once per server:
#   1. Build graft-bench and graft-resolve (tools/graft-agents/cmd/graft-resolve)
#      and install them on the server host, for example in /var/lib/graft/bin.
#   2. Copy examples/graft/demo/resolve.toml to /etc/graft/resolve.toml and add
#      this to the [graft] section of the server configuration:
#
#        [graft.land.resolver]
#        enabled = true
#        command = ["/var/lib/graft/bin/graft-resolve", "--config", "/etc/graft/resolve.toml"]
#        paths_allow = ["shared/log.txt"]
#
#      Without it, land-resolve resolves nothing: its conflicting changes count
#      as failed and the resolutions column is 0. See doc/graft/RESOLVE.md.
#
# The repository is created on the server by the first run (--create-repo),
# through the server's RepositoryService. Nothing is written into the storage
# directory by hand.
#
# Requirements: jq and column on PATH; graft-bench built (or set GRAFT_BENCH).
#
# Usage:
#   GRAFT_ADDRESS=unix:/var/lib/graft/run/gitaly.socket \
#   GRAFT_TOKEN=... sh examples/graft/demo/demo.sh
#
# Tunables (environment): AGENTS (50), CHANGES (10 per agent), RATE (0.2 of
# changes touch the shared file), TEST_MS (200), GRAFT_BENCH, GRAFT_STORAGE
# (default), DEMO_REPO (new repository name), OUT (output directory, default a
# new temporary directory).

set -eu

ADDRESS=${GRAFT_ADDRESS:?set GRAFT_ADDRESS, for example unix:/var/lib/graft/run/gitaly.socket}
: "${GRAFT_TOKEN:?set GRAFT_TOKEN to the shared token of the server}"
STORAGE=${GRAFT_STORAGE:-default}
AGENTS=${AGENTS:-50}
CHANGES=${CHANGES:-10}
RATE=${RATE:-0.2}
TEST_MS=${TEST_MS:-200}
BENCH=${GRAFT_BENCH:-graft-bench}
OUT=${OUT:-$(mktemp -d)}
REPO=${DEMO_REPO:-bench-$(date +%s).git}

for tool in jq column; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "demo: $tool is required" >&2
		exit 1
	}
done

first=1
for mode in land land-resolve queue; do
	extra=""
	if [ "$mode" = queue ]; then
		extra="--test-duration ${TEST_MS}ms"
	fi
	create=""
	if [ "$first" = 1 ]; then
		echo "demo: creating repository $REPO on storage $STORAGE"
		create="--create-repo"
		first=0
	fi

	echo "demo: running mode $mode with $AGENTS agents"
	# shellcheck disable=SC2086
	if ! "$BENCH" --address "$ADDRESS" --storage "$STORAGE" --repo "$REPO" $create \
		--branch "bench-$mode" --agents "$AGENTS" --changes "$CHANGES" \
		--conflict-rate "$RATE" --mode "$mode" --format json $extra \
		>"$OUT/$mode.json" 2>"$OUT/$mode.err"; then
		echo "demo: mode $mode failed: $(cat "$OUT/$mode.err")" >&2
		rm -f "$OUT/$mode.json"
	fi
done

{
	printf 'mode\tlanded\tfailed\tconflicts\tresolutions\tlands/min\tp50 ms\tp95 ms\tp99 ms\n'
	for mode in land land-resolve queue; do
		[ -s "$OUT/$mode.json" ] || continue
		jq -r '[.mode, .landed, .failed, .conflicts, .resolutions,
			(.lands_per_minute * 10 | round / 10), (.p50_ms * 10 | round / 10),
			(.p95_ms * 10 | round / 10), (.p99_ms * 10 | round / 10)] | @tsv' "$OUT/$mode.json"
	done
} | column -t -s "$(printf '\t')"

echo "demo: raw results in $OUT"
