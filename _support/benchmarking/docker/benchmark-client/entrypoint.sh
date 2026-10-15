#!/bin/sh
# Entry point for the in-cluster benchmark Job.
#
# Reads its configuration from environment variables (set on the Job spec) and
# from files mounted by a ConfigMap at /etc/benchmark/:
#   - /etc/benchmark/k6-benchmark.js : the workload script
#   - /etc/benchmark/repositories.json : test data manifest
#   - /etc/benchmark/plot.py : optional pandas/plotnine post-processor;
#                              if present and non-empty, runs after k6 against
#                              each harvested gitaly-<N>.log to produce PNGs
#                              under plots/gitaly-<N>/.
#
# Env vars read by this script:
#   RUN_NAME              - unique identifier for this run (required)
#   PROFILE               - "true"/"false", default true. When true, scrape
#                           Go pprof profiles from each Gitaly pod's metrics
#                           endpoint (HTTP, no in-pod execution required).
#   PROFILE_DURATION      - seconds to profile (CPU + execution trace), default 120
#   GITALY_NAMESPACE      - namespace containing the Gitaly pod, default "gitlab"
#   GITALY_SERVICE        - Service name the benchmark targets; its Endpoints
#                           determine which pods' logs are harvested
#   GITALY_POD_SELECTOR   - label selector for the Gitaly pod, default "app=gitaly"
#                           (pprof scraping, and log-harvest fallback when
#                           GITALY_SERVICE is unset/unresolvable)
#   GITALY_CONTAINER      - container name within the pod, default "gitaly"
#   GITALY_METRICS_PORT   - Gitaly Prometheus / pprof port, default 9236
#
# Other env vars on the container (GITALY_ADDRESS, GITALY_PROTO_DIR,
# WORKLOAD_DURATION, GITALY_TOKEN) are consumed by k6-benchmark.js, not this
# script; see that file for their meaning.
#
# On exit (success or failure) this script touches the file
# /tmp/run/.done so the GCS uploader sidecar can finalize the run and exit.

set -eu

RESULTS_DIR="/tmp/run/${RUN_NAME}"
mkdir -p "${RESULTS_DIR}"

cleanup() {
    touch /tmp/run/.done
}
trap cleanup EXIT

cp /etc/benchmark/repositories.json /opt/benchmark-gitaly/repositories.json
cp /etc/benchmark/k6-benchmark.js   /opt/benchmark-gitaly/k6-benchmark.js
if [ -s /etc/benchmark/plot.py ]; then
    cp /etc/benchmark/plot.py /opt/benchmark-gitaly/plot.py
fi

PROFILE="${PROFILE:-true}"
PROFILE_DURATION="${PROFILE_DURATION:-120}"
GITALY_NAMESPACE="${GITALY_NAMESPACE:-gitlab}"
GITALY_POD_SELECTOR="${GITALY_POD_SELECTOR:-app=gitaly}"
GITALY_CONTAINER="${GITALY_CONTAINER:-gitaly}"
GITALY_METRICS_PORT="${GITALY_METRICS_PORT:-9236}"

# ssh transport: materialize the key from the Secret at 0600 (ssh refuses
# one others can read). No key leaves the command unset, failing loudly.
GITLAB_SSH_URL="${GITLAB_SSH_URL:-ssh://git@gitlab-gitlab-shell.${GITALY_NAMESPACE}.svc:22}"
export GITLAB_SSH_URL
if [ -n "${BENCH_SSH_KEY:-}" ]; then
    bench_ssh_key=/opt/benchmark-gitaly/ssh/id_ed25519
    mkdir -p "$(dirname "${bench_ssh_key}")"
    printf '%s\n' "${BENCH_SSH_KEY}" > "${bench_ssh_key}"
    chmod 0600 "${bench_ssh_key}"
    BENCH_SSH_COMMAND="ssh -i ${bench_ssh_key} -o BatchMode=yes"
    BENCH_SSH_COMMAND="${BENCH_SSH_COMMAND} -o StrictHostKeyChecking=no"
    BENCH_SSH_COMMAND="${BENCH_SSH_COMMAND} -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
    export BENCH_SSH_COMMAND
    echo "[entrypoint] ssh transport armed against ${GITLAB_SSH_URL}" >&2
else
    echo "[entrypoint] WARNING: BENCH_SSH_KEY unset; ssh transport unavailable" >&2
fi

# Shallow-clone every test repository once: a real `git push` needs local
# history to negotiate against. Anonymous HTTP works because the bench
# projects are public. A failed clone only fails that repo's push checks.
CLONES_DIR="${CLONES_DIR:-/opt/benchmark-gitaly/clones}"
GITLAB_HTTP_URL="${GITLAB_HTTP_URL:-http://gitlab-webservice-default.${GITALY_NAMESPACE}.svc:8181}"
BENCH_GROUP="${BENCH_GROUP:-bench}"
export CLONES_DIR GITLAB_HTTP_URL BENCH_GROUP
mkdir -p "${CLONES_DIR}"
jq -r '.[] | select(.include_in_test) | .name' \
    /opt/benchmark-gitaly/repositories.json |
while read -r repo_name; do
    [ -n "${repo_name}" ] || continue
    echo "[entrypoint] cloning ${BENCH_GROUP}/${repo_name} for push sections" >&2
    git clone --quiet --bare --depth 1 \
        "${GITLAB_HTTP_URL}/${BENCH_GROUP}/${repo_name}.git" \
        "${CLONES_DIR}/${repo_name}.git" ||
        echo "[entrypoint] clone of ${repo_name} failed; its push sections will fail" >&2
done

# Scrape Go pprof from each Gitaly pod over HTTP; replaces the VM flow's
# profile-gitaly, which needed SYS_PTRACE the chart doesn't grant. Pods
# are hit directly (the Service VIP would round-robin). CPU and trace run
# alongside k6; heap/goroutine/allocs/mutex/block snapshots follow after.
profile_pid=""
if [ "${PROFILE}" = "true" ]; then
    mkdir -p "${RESULTS_DIR}/pprof"
    pods="$(kubectl --namespace "${GITALY_NAMESPACE}" \
        get pods -l "${GITALY_POD_SELECTOR}" \
        -o jsonpath='{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}' \
        2>/dev/null)"
    if [ -z "${pods}" ]; then
        echo "[entrypoint] no Gitaly pods found for selector '${GITALY_POD_SELECTOR}' in ns '${GITALY_NAMESPACE}'" >&2
    else
        echo "${pods}" > "${RESULTS_DIR}/pprof/targets.txt"
        echo "[entrypoint] scraping pprof from $(echo "${pods}" | wc -l) Gitaly pod(s) for ${PROFILE_DURATION}s" >&2
        (
            set +e
            echo "${pods}" | while read -r pod ip; do
                [ -n "${pod}" ] || continue
                [ -n "${ip}" ] || continue
                base="http://${ip}:${GITALY_METRICS_PORT}/debug/pprof"
                out="${RESULTS_DIR}/pprof/${pod}"
                mkdir -p "${out}"
                curl -fsS --max-time "$((PROFILE_DURATION + 30))" \
                    -o "${out}/cpu.pprof" \
                    "${base}/profile?seconds=${PROFILE_DURATION}" \
                    2> "${out}/cpu.err" &
                curl -fsS --max-time "$((PROFILE_DURATION + 30))" \
                    -o "${out}/trace.out" \
                    "${base}/trace?seconds=${PROFILE_DURATION}" \
                    2> "${out}/trace.err" &
            done
            wait
        ) &
        profile_pid=$!
    fi
fi

# Stream the Gitaly pods' logs instead of harvesting afterwards: the
# kubelet rotates container logs at 10MB, which a loaded Gitaly exceeds,
# so a post-run harvest only sees the tail of the run. --tail=-1 also
# keeps the pre-run buffer.
gitaly_log_pods=""
if [ -n "${GITALY_SERVICE:-}" ]; then
    gitaly_log_pods="$(kubectl --namespace "${GITALY_NAMESPACE}" \
        get endpoints "${GITALY_SERVICE}" \
        -o jsonpath='{range .subsets[*]}{range .addresses[*]}{.targetRef.name}{"\n"}{end}{range .notReadyAddresses[*]}{.targetRef.name}{"\n"}{end}{end}' \
        2>/dev/null | grep . | sort -u || true)"
    if [ -z "${gitaly_log_pods}" ]; then
        echo "[entrypoint] could not resolve pods from endpoints of service '${GITALY_SERVICE}'; falling back to selector '${GITALY_POD_SELECTOR}' (may include other Gitaly deployments)" >&2
    fi
fi
if [ -z "${gitaly_log_pods}" ]; then
    gitaly_log_pods="$(kubectl --namespace "${GITALY_NAMESPACE}" \
        get pods -l "${GITALY_POD_SELECTOR}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
        2>/dev/null | sort)"
fi
log_follow_pids=""
if [ -z "${gitaly_log_pods}" ]; then
    echo "[entrypoint] no Gitaly pods found for service '${GITALY_SERVICE:-}' or selector '${GITALY_POD_SELECTOR}' in ns '${GITALY_NAMESPACE}'; skipping log capture" >&2
else
    i=1
    for pod in ${gitaly_log_pods}; do
        echo "[entrypoint] streaming logs from ${pod} -> gitaly-${i}.log" >&2
        printf 'gitaly-%s.log %s\n' "${i}" "${pod}" \
            >>"${RESULTS_DIR}/gitaly-log-pods.txt"
        kubectl --namespace "${GITALY_NAMESPACE}" logs "${pod}" \
            -c "${GITALY_CONTAINER}" --tail=-1 --follow \
            >"${RESULTS_DIR}/gitaly-${i}.log" 2>&1 &
        log_follow_pids="${log_follow_pids} $!"
        i=$((i + 1))
    done
fi

echo "[entrypoint] running k6 against ${GITALY_ADDRESS}" >&2
set +e
k6 run \
    --verbose \
    --summary-mode=full \
    --summary-export="${RESULTS_DIR}/k6-summary.json" \
    --console-output="${RESULTS_DIR}/k6-console.txt" \
    --log-output="file=${RESULTS_DIR}/k6-log.txt" \
    /opt/benchmark-gitaly/k6-benchmark.js
k6_status=$?
set -e

if [ -n "${profile_pid}" ]; then
    wait "${profile_pid}" || true
fi

if [ "${PROFILE}" = "true" ] && [ -n "${pods:-}" ]; then
    echo "[entrypoint] capturing point-in-time pprof snapshots" >&2
    echo "${pods}" | while read -r pod ip; do
        [ -n "${pod}" ] || continue
        [ -n "${ip}" ] || continue
        base="http://${ip}:${GITALY_METRICS_PORT}/debug/pprof"
        out="${RESULTS_DIR}/pprof/${pod}"
        mkdir -p "${out}"
        for kind in heap goroutine allocs mutex block; do
            curl -fsS --max-time 30 \
                -o "${out}/${kind}.pprof" \
                "${base}/${kind}" \
                2> "${out}/${kind}.err" || true
        done
    done
fi

# Stop the log followers started before k6 (short grace so trailing
# lines land in the files first).
if [ -n "${log_follow_pids}" ]; then
    echo "[entrypoint] stopping Gitaly log streams" >&2
    sleep 5
    for pid in ${log_follow_pids}; do
        kill "${pid}" 2>/dev/null || true
    done
    # shellcheck disable=SC2086 # deliberate word split of the PID list
    wait ${log_follow_pids} 2>/dev/null || true
fi

# Per-RPC latency stats from the harvested logs: Gitaly's server-side
# grpc.time_ms, unlike k6's client-side wall-clock numbers. Gitaly's log
# fields are flat keys containing literal dots, so the queries need agrind's
# ["..."] syntax - bare grpc.time_ms is a nested lookup and matches nothing.
# The "grpc.time_ms" prefilter and null guard drop start-of-call lines,
# which carry grpc.method but no duration.
AGRIND_LATENCY_QUERY='"grpc.time_ms"
    | json
    | where ["grpc.time_ms"] != null
    | avg(["grpc.time_ms"]),
      min(["grpc.time_ms"]),
      p50(["grpc.time_ms"]) as med,
      max(["grpc.time_ms"]),
      p90(["grpc.time_ms"]),
      p95(["grpc.time_ms"])
      by ["grpc.method"]
    | sort by p95 desc'
# Ungrouped variant: the report's all-RPCs percentiles cannot be recombined
# from the per-RPC rows, so they get their own pass over the raw samples.
AGRIND_OVERALL_QUERY='"grpc.time_ms"
    | json
    | where ["grpc.time_ms"] != null
    | avg(["grpc.time_ms"]),
      min(["grpc.time_ms"]),
      p50(["grpc.time_ms"]) as med,
      max(["grpc.time_ms"]),
      p90(["grpc.time_ms"]),
      p95(["grpc.time_ms"])'
AGRIND_CODE_QUERY='"grpc.code" | json
| where ["grpc.code"] != null
| count by ["grpc.method"], ["grpc.code"]
| sort by _count desc'

grpc_stats() {
    stats_log=$1
    stats_out=$2
    agrind --file "${stats_log}" "${AGRIND_LATENCY_QUERY}" \
        > "${stats_out}-latency.txt" 2>/dev/null || true
    agrind --file "${stats_log}" -o json "${AGRIND_LATENCY_QUERY}" \
        > "${stats_out}-latency.json" 2>/dev/null || true
    agrind --file "${stats_log}" -o json "${AGRIND_OVERALL_QUERY}" \
        > "${stats_out}-latency-overall.json" 2>/dev/null || true
    agrind --file "${stats_log}" "${AGRIND_CODE_QUERY}" \
        > "${stats_out}-codes.txt" 2>/dev/null || true
}

if command -v agrind >/dev/null 2>&1; then
    stats_dir="${RESULTS_DIR}/gitaly-stats"
    mkdir -p "${stats_dir}"
    for log in "${RESULTS_DIR}"/gitaly-*.log; do
        [ -s "${log}" ] || continue
        name="$(basename "${log}" .log)"
        echo "[entrypoint] computing gRPC stats from ${name}.log" >&2
        grpc_stats "${log}" "${stats_dir}/${name}"
    done
    combined_log="/tmp/gitaly-combined.log"
    cat "${RESULTS_DIR}"/gitaly-*.log > "${combined_log}" 2>/dev/null || true
    if [ -s "${combined_log}" ]; then
        echo "[entrypoint] computing combined gRPC stats across all Gitaly logs" >&2
        grpc_stats "${combined_log}" "${stats_dir}/all-gitalies"
    fi
    rm -f "${combined_log}"
else
    echo "[entrypoint] agrind not found; skipping gRPC log stats" >&2
fi

if [ -f /opt/benchmark-gitaly/plot.py ]; then
    for log in "${RESULTS_DIR}"/gitaly-*.log; do
        [ -s "${log}" ] || continue
        name="$(basename "${log}" .log)"
        plot_dir="${RESULTS_DIR}/plots/${name}"
        echo "[entrypoint] generating plots from ${name}.log" >&2
        mkdir -p "${plot_dir}"
        python3 /opt/benchmark-gitaly/plot.py \
            "${log}" \
            "${plot_dir}" \
            >"${plot_dir}/plot-stdout.log" \
            2>"${plot_dir}/plot-stderr.log" ||
            echo "[entrypoint] plot.py failed for ${name}.log (non-fatal; see plots/${name}/plot-stderr.log)" >&2
    done
fi

echo "[entrypoint] k6 exited with status ${k6_status}" >&2
exit "${k6_status}"
