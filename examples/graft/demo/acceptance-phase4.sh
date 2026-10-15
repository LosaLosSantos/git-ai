#!/usr/bin/env bash
# Phase 4a acceptance run: runs bench-batch.sh and checks the acceptance
# criteria of doc/graft/PLAN4.md (section Acceptance) against its JSON output.
#
#   1. attempts/landed = 1.00 with batching on, at every agent count.
#   2. Failures are planned conflicts only: with batching on, failed = conflicts.
#   3. p99 of landed changes with batching on <= p99 with batching off, at every
#      agent count.
#   Stretch (reported, not required): p99(on-100) <= 0.5 * p99(off-100).
#
# Prints one PASS or FAIL line per criterion and exits non-zero on any FAIL.
#
# Usage:
#   bash examples/graft/demo/acceptance-phase4.sh [WORKDIR]
#   bash examples/graft/demo/acceptance-phase4.sh --dry-run WORKDIR
#
# --dry-run builds and starts nothing: it only checks the JSON results that are
# already in WORKDIR/results (the files of bench-batch.sh). It is how the checker
# is tested without a benchmark run.
#
# The environment variables of bench-batch.sh (AGENT_COUNTS, CHANGES, RATE,
# SEED) are passed through. Requires python3 besides what bench-batch.sh needs.
#
# The results (JSON, Markdown, Prometheus scrapes and the combined table) stay in
# WORKDIR/results. On exit the script stops any Gitaly server of WORKDIR that is
# still running, and deletes the agent tokens in WORKDIR/tokens.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

dry_run=0
if [ "${1:-}" = "--dry-run" ]; then
  dry_run=1
  shift
  if [ "$#" -ne 1 ]; then
    echo "acceptance-phase4: --dry-run needs the WORKDIR of a previous run" >&2
    exit 2
  fi
fi

work="${1:-$(mktemp -d)}"
out="$work/results"
AGENT_COUNTS=${AGENT_COUNTS:-"20 50 100"}
RATE=${RATE:-0.2}
SEED=${SEED:-1}

# cleanup runs on every exit. It stops the Gitaly servers that a killed
# bench-batch.sh may have left behind, and removes the agent tokens, which are
# secrets. The results are kept.
cleanup() {
  local status=$?

  if command -v pkill >/dev/null 2>&1; then
    pkill -f -- "$work/bin/gitaly serve" 2>/dev/null || true
  fi
  rm -rf "$work/tokens"

  if [ "$status" -ne 0 ]; then
    echo "acceptance-phase4: results kept in $out" >&2
  fi
}
trap cleanup EXIT

if [ "$dry_run" -eq 0 ]; then
  if ! bash "$repo_root/examples/graft/demo/bench-batch.sh" "$work"; then
    echo "acceptance-phase4: FAIL (bench-batch.sh exited non-zero)"
    exit 1
  fi
fi

echo
echo "acceptance-phase4: checking $out"

status=0
python3 -I - "$out" "$AGENT_COUNTS" <<'PY' || status=$?
import json
import os
import sys

out, counts = sys.argv[1], sys.argv[2].split()
failed = False


def load(name, agents):
    path = os.path.join(out, "%s-%s.json" % (name, agents))
    with open(path) as f:
        return json.load(f)


def report(ok, text):
    global failed
    print(("PASS " if ok else "FAIL ") + text)
    if not ok:
        failed = True


def value(d, key):
    # A missing or non-numeric field is None, and a criterion that needs it fails.
    v = d.get(key)
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return None
    return float(v)


def fmt(v):
    return "missing" if v is None else "%.1f" % v


runs = {}
for agents in counts:
    try:
        runs[agents] = (load("off", agents), load("on", agents))
    except (OSError, ValueError) as err:
        report(False, "results for %s agents: %s" % (agents, err))

for agents in counts:
    if agents not in runs:
        continue
    off, on = runs[agents]
    landed = on.get("landed", 0)

    ok = landed > 0 and abs(on.get("attempts_per_land", 0.0) - 1.0) < 0.005
    report(ok, "attempts/landed = %.2f with batching on, %s agents (landed %d)"
           % (on.get("attempts_per_land", 0.0), agents, landed))

    ok = landed > 0 and on.get("failed", 0) == on.get("conflicts", -1)
    report(ok, "failures are planned conflicts only, batching on, %s agents "
               "(failed %d, conflicts %d)"
           % (agents, on.get("failed", 0), on.get("conflicts", -1)))

    p99_on, p99_off = value(on, "p99_ms"), value(off, "p99_ms")
    ok = landed > 0 and p99_on is not None and p99_off is not None and p99_on <= p99_off
    report(ok, "p99 on %s ms <= off %s ms, %s agents" % (fmt(p99_on), fmt(p99_off), agents))

if "100" in runs:
    off, on = runs["100"]
    p99_on, p99_off = value(on, "p99_ms"), value(off, "p99_ms")
    if p99_on is None or p99_off is None:
        print("STRETCH MISS p99 missing in the results for 100 agents")
    else:
        ok = p99_on <= 0.5 * p99_off
        print(("STRETCH PASS " if ok else "STRETCH MISS ")
              + "p99 on-100 %.1f ms, 0.5 x off-100 %.1f ms" % (p99_on, 0.5 * p99_off))

sys.exit(1 if failed else 0)
PY

if [ "$status" -ne 0 ]; then
  echo "acceptance-phase4: FAIL"
  exit 1
fi
echo "acceptance-phase4: PASS"
