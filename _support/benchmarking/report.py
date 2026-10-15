#!/usr/bin/env python3
"""Render a Markdown summary from a k6 benchmark run.

Reads ``k6-summary.json`` produced by ``k6 run --summary-export`` and emits a
human-readable report to stdout (or ``--output``). The report is intended for
posting as a GitLab MR note.

Optional inputs in the same results directory are linked in if present:

  - ``gitaly-stats/all-gitalies-latency{,-overall}.json`` - server-side
    per-RPC latency aggregated by agrind from the harvested Gitaly logs;
    the sole latency source (absent stats render as empty/zero)
  - ``plots/*.png``       - PNG plots produced by an experiment's plot.py

The ``--artifact-base-url`` flag, if set, turns relative plot paths into
clickable links suitable for embedding in an MR comment. Typically passed as
``--artifact-base-url=${CI_JOB_URL}/artifacts/raw/_support/benchmarking/experiments/${EXPERIMENT}/results/${RUN_NAME}``.

This script intentionally has zero non-stdlib dependencies so it can run in any
CI image without ``pip install``.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

# Latency stat labels exposed by the report, mapped to the keys agrind's
# -o json output uses for them.
_STAT_LABELS = ["p50", "p90", "p95", "max", "avg"]
_AGRIND_STAT_KEYS = {"p50": "med", "p90": "p90", "p95": "p95",
                     "max": "_max", "avg": "_average"}
# agrind emits the group-by column under the escaped field-reference key.
_AGRIND_RPC_KEY = '["grpc.method"]'


def fmt_ms(value_ms: float) -> str:
    """Render a millisecond value compactly. k6 reports durations in ms."""
    if value_ms < 1:
        return f"{value_ms * 1000:.0f}\u00b5s"
    if value_ms < 1000:
        return f"{value_ms:.1f}ms"
    return f"{value_ms / 1000:.2f}s"


def fmt_rate(value_per_s: float) -> str:
    if value_per_s >= 1000:
        return f"{value_per_s / 1000:.1f}k/s"
    return f"{value_per_s:.1f}/s"


def fmt_pct(numerator: int, denominator: int) -> str:
    if denominator == 0:
        return "n/a"
    return f"{(numerator / denominator) * 100:.1f}%"


def _normalize_stats(row: dict, keys: dict) -> dict:
    return {label: row.get(key, 0) for label, key in keys.items()}


def _load_agrind_rows(path: Path) -> list:
    if not path.is_file():
        return []
    try:
        rows = json.loads(path.read_text())
    except (OSError, ValueError):
        return []
    return rows if isinstance(rows, list) else []


def agrind_latency(results_dir: Path) -> tuple[dict, dict]:
    """(overall, per-RPC) latency from the agrind stats in gitaly-stats/.

    These are Gitaly's server-side ``grpc.time_ms`` figures aggregated
    across every harvested pod log by the benchmark entrypoint. Returns
    ``({}, {})`` when the run has no agrind stats.
    """
    stats_dir = results_dir / "gitaly-stats"
    overall = {}
    rows = _load_agrind_rows(stats_dir / "all-gitalies-latency-overall.json")
    if rows:
        overall = _normalize_stats(rows[0], _AGRIND_STAT_KEYS)
    per_rpc = {}
    for row in _load_agrind_rows(stats_dir / "all-gitalies-latency.json"):
        rpc = row.get(_AGRIND_RPC_KEY)
        if rpc:
            per_rpc[rpc] = _normalize_stats(row, _AGRIND_STAT_KEYS)
    return overall, per_rpc


def per_rpc_checks(root_group: dict) -> dict:
    """Aggregate check passes/fails by check-name prefix.

    Names follow ``"<prefix> - <description>"``; a prefix may have several
    checks (e.g. UserMergeBranch), so sum them by prefix.
    """
    out: dict[str, dict] = {}
    for name, data in root_group.get("checks", {}).items():
        rpc = name.split(" - ", 1)[0].strip()
        entry = out.setdefault(rpc, {"passes": 0, "fails": 0})
        entry["passes"] += data.get("passes", 0)
        entry["fails"] += data.get("fails", 0)
    return out


def _om_value(value: float) -> str:
    f = float(value)
    if f == int(f):
        return str(int(f))
    return f"{f:.6g}"


def _om_labels(labels: dict) -> str:
    parts = []
    for key, val in labels.items():
        if val is None:
            continue
        escaped = str(val).replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")
        parts.append(f'{key}="{escaped}"')
    return "{" + ",".join(parts) + "}" if parts else ""


def render_openmetrics(metrics: dict, root_group: dict,
                       overall: dict, per_rpc: dict,
                       experiment: str | None,
                       gitaly_version: str | None = None) -> str:
    """Render the metrics in the OpenMetrics text format.

    Consumed by GitLab's ``artifacts:reports:metrics``, which renders the
    values in the MR widget and diffs each ``name{labels}`` line against the
    target branch's latest run.

    Conforms to the OpenMetrics text format: every family is preceded by
    ``# TYPE`` (and ``# HELP``), a family's samples are contiguous, counters
    carry the ``_total`` suffix (the family name in ``# TYPE`` omits it), and
    the exposition is terminated by ``# EOF``.

    Latency is emitted overall (no ``rpc`` label) and per RPC (``rpc`` label)
    in one gauge family, distinguished by a ``stat`` label
    (p50/p90/p95/max/avg). Check pass ratio is likewise overall and per RPC.
    """
    base = {"experiment": experiment} if experiment else {}
    iters = metrics.get("iterations", {})
    checks = metrics.get("checks", {})
    dropped = metrics.get("dropped_iterations", {})

    def latency_samples(stat_source: dict, extra: dict | None = None) -> list:
        extra = extra or {}
        return [(dict(base, **extra, stat=label), stat_source.get(label, 0))
                for label in _STAT_LABELS]

    duration = latency_samples(overall) if overall else []
    for rpc, stats in sorted(per_rpc.items()):
        duration += latency_samples(stats, {"rpc": rpc})

    passes = checks.get("passes", 0)
    fails = checks.get("fails", 0)
    total = passes + fails
    pass_ratio = [(dict(base), passes / total if total else 0.0)]
    rpc_passes, rpc_fails = [], []
    for rpc, counts in sorted(per_rpc_checks(root_group).items()):
        p, f = counts["passes"], counts["fails"]
        t = p + f
        rpc_passes.append((dict(base, rpc=rpc), p))
        rpc_fails.append((dict(base, rpc=rpc), f))
        pass_ratio.append((dict(base, rpc=rpc), p / t if t else 0.0))

    # (family name, type, help, samples, sample name [defaults to family]).
    families = [
        ("gitaly_rpc_duration_ms", "gauge",
         "Server-side RPC latency in milliseconds from the Gitaly logs "
         "(stat=p50|p90|p95|max|avg).",
         duration, None),
        ("gitaly_benchmark_iterations", "counter",
         "Completed k6 iterations.",
         [(dict(base), iters.get("count", 0))],
         "gitaly_benchmark_iterations_total"),
        ("gitaly_benchmark_iterations_per_second", "gauge",
         "k6 iteration throughput per second.",
         [(dict(base), iters.get("rate", 0))], None),
        ("gitaly_benchmark_dropped_iterations", "counter",
         "k6 iterations dropped before starting.",
         [(dict(base), dropped.get("count", 0))],
         "gitaly_benchmark_dropped_iterations_total"),
        ("gitaly_benchmark_checks_pass_ratio", "gauge",
         "Fraction of k6 checks passing (overall and per RPC).",
         pass_ratio, None),
        ("gitaly_benchmark_checks_passes", "counter",
         "k6 checks passed per RPC.", rpc_passes,
         "gitaly_benchmark_checks_passes_total"),
        ("gitaly_benchmark_checks_fails", "counter",
         "k6 checks failed per RPC.", rpc_fails,
         "gitaly_benchmark_checks_fails_total"),
    ]
    if gitaly_version:
        families.append(
            ("gitaly_benchmark_build_info", "gauge",
             "Deployed Gitaly under test; value is always 1.",
             [(dict(base, gitaly_version=gitaly_version), 1)], None))

    lines = []
    for name, mtype, help_text, samples, sample_name in families:
        if not samples:
            continue
        lines.append(f"# HELP {name} {help_text}")
        lines.append(f"# TYPE {name} {mtype}")
        for labels, value in samples:
            lines.append(
                f"{sample_name or name}{_om_labels(labels)} {_om_value(value)}")
    lines.append("# EOF")
    return "\n".join(lines) + "\n"


def _pct_delta_value(current: float, baseline: float) -> float | None:
    """Signed % change; callers pass (B, A). None when baseline is 0."""
    if baseline == 0:
        return None
    return (current - baseline) / baseline * 100


def _pct_delta(current: float, baseline: float) -> str:
    """Signed %-change string, e.g. ``+12.3%``."""
    delta = _pct_delta_value(current, baseline)
    if delta is None:
        return "n/a" if current == 0 else "new"
    return f"{delta:+.1f}%"


# Deltas below this (% or %-points) count as noise, not a win.
_VERDICT_EPS = 5.0


def _verdict(delta: float | None, higher_is_better: bool,
             eps: float = _VERDICT_EPS) -> str:
    """Marker saying whether B beats A. ``delta`` is relative % for ratio
    metrics, %-points for pass rates; "" when undefined."""
    if delta is None:
        return ""
    if abs(delta) < eps:
        return "≈ similar"
    improved = delta > 0 if higher_is_better else delta < 0
    return "✅ better" if improved else "🔴 worse"


def render_compare_summary_table(metrics_a: dict, metrics_b: dict,
                                 lat_a: dict, lat_b: dict,
                                 label_a: str, label_b: str) -> str:
    """Side-by-side aggregate metrics for two runs (A vs B)."""
    iters_a = metrics_a.get("iterations", {})
    iters_b = metrics_b.get("iterations", {})
    checks_a = metrics_a.get("checks", {})
    checks_b = metrics_b.get("checks", {})

    def pass_pct(checks: dict) -> str:
        return fmt_pct(checks.get("passes", 0),
                       checks.get("passes", 0) + checks.get("fails", 0))

    def pass_ratio(checks: dict) -> float | None:
        total = checks.get("passes", 0) + checks.get("fails", 0)
        return checks.get("passes", 0) / total if total else None

    ra, rb = pass_ratio(checks_a), pass_ratio(checks_b)
    # Pass-rate delta is in %-points, not relative %; leave Δ blank but judge.
    checks_pts = (rb - ra) * 100 if ra is not None and rb is not None else None

    rows = [
        ("Throughput",
         fmt_rate(iters_a.get("rate", 0)), fmt_rate(iters_b.get("rate", 0)),
         _pct_delta(iters_b.get("rate", 0), iters_a.get("rate", 0)),
         _verdict(_pct_delta_value(iters_b.get("rate", 0),
                                   iters_a.get("rate", 0)), True)),
        ("Iterations",
         f"{iters_a.get('count', 0):,}", f"{iters_b.get('count', 0):,}",
         _pct_delta(iters_b.get("count", 0), iters_a.get("count", 0)),
         _verdict(_pct_delta_value(iters_b.get("count", 0),
                                   iters_a.get("count", 0)), True)),
        ("Checks passed", pass_pct(checks_a), pass_pct(checks_b), "",
         _verdict(checks_pts, True)),
    ]
    for stat_label in _STAT_LABELS:
        a_val, b_val = lat_a.get(stat_label, 0), lat_b.get(stat_label, 0)
        rows.append(
            (f"RPC latency {stat_label}", fmt_ms(a_val), fmt_ms(b_val),
             _pct_delta(b_val, a_val),
             _verdict(_pct_delta_value(b_val, a_val), higher_is_better=False)))

    lines = [f"| Metric | {label_a} | {label_b} | Δ (B vs A) | {label_b} better? |",
             "| --- | ---: | ---: | ---: | :--- |"]
    lines.extend(f"| {name} | {a} | {b} | {d} | {v} |"
                 for name, a, b, d, v in rows)
    return "\n".join(lines)


def render_compare_rpc_latency_table(per_a: dict, per_b: dict,
                                     label_a: str, label_b: str) -> str:
    """Per-RPC p95 latency, B vs A, sorted alphabetically by RPC name."""
    rpcs = sorted(set(per_a) | set(per_b))
    if not rpcs:
        return ""

    def p95(stats: dict) -> float:
        return stats.get("p95", 0)

    rows = []
    for rpc in rpcs:
        a = p95(per_a.get(rpc, {}))
        b = p95(per_b.get(rpc, {}))
        # Zero p95 = no sample on that side; nothing to compare.
        comparable = a > 0 and b > 0
        rows.append((rpc, a, b, comparable))

    lines = ["### Per-RPC latency (p95)", "",
             f"| RPC | {label_a} | {label_b} | Δ (B vs A) | {label_b} better? |",
             "| --- | ---: | ---: | ---: | :--- |"]
    for rpc, a, b, comparable in rows:
        if comparable:
            delta = _pct_delta(b, a)
            verdict = _verdict(_pct_delta_value(b, a), higher_is_better=False)
        else:
            # A sample on at most one side; "new" when it's B's.
            delta, verdict = ("new" if b > 0 else "n/a"), ""
        lines.append(
            f"| {rpc} | {fmt_ms(a)} | {fmt_ms(b)} | {delta} | {verdict} |")
    return "\n".join(lines)


def render_compare_checks_table(root_group_a: dict, root_group_b: dict,
                                label_a: str, label_b: str) -> str:
    """k6 check pass rates grouped by check-name prefix, B vs A."""
    per_a = per_rpc_checks(root_group_a)
    per_b = per_rpc_checks(root_group_b)
    rpcs = sorted(set(per_a) | set(per_b))
    if not rpcs:
        return ""

    def rate(counts: dict) -> float | None:
        total = counts.get("passes", 0) + counts.get("fails", 0)
        return counts.get("passes", 0) / total if total else None

    rows = []
    for rpc in rpcs:
        a = rate(per_a.get(rpc, {}))
        b = rate(per_b.get(rpc, {}))
        rows.append((rpc, a, b))

    lines = ["### Check pass rate", "",
             "k6's client-side checks, grouped by check-name prefix: a "
             "workload section (e.g. `pushSmallCommit-ssh`), an RPC, or "
             "`<section> <RPC>` where a section checks one RPC. `n/a` means "
             "that side did not run the check.", "",
             f"| Check | {label_a} | {label_b} | Δ (%points) | {label_b} better? |",
             "| --- | ---: | ---: | ---: | :--- |"]
    for rpc, a, b in rows:
        a_s = f"{a * 100:.1f}%" if a is not None else "n/a"
        b_s = f"{b * 100:.1f}%" if b is not None else "n/a"
        if a is not None and b is not None:
            pts = (b - a) * 100
            d_s = f"{pts:+.1f}"
            v_s = _verdict(pts, higher_is_better=True)
        else:
            d_s, v_s = "n/a", ""
        lines.append(f"| {rpc} | {a_s} | {b_s} | {d_s} | {v_s} |")
    return "\n".join(lines)


def render_compare_report(summary_a: dict, summary_b: dict,
                          latency_a: tuple[dict, dict],
                          latency_b: tuple[dict, dict],
                          label_a: str, label_b: str,
                          experiment: str | None,
                          version_a: str | None, version_b: str | None,
                          run_a: str | None = None,
                          run_b: str | None = None,
                          grafana: str | None = None) -> str:
    """A/B comparison note: each metric gets a direction-aware "B better?"
    marker (B = --compare side)."""
    metrics_a = summary_a.get("metrics", {})
    metrics_b = summary_b.get("metrics", {})
    overall_a, per_rpc_a = latency_a
    overall_b, per_rpc_b = latency_b

    def side(letter: str, label: str, run: str | None, version: str | None) -> str:
        parts = [f"**{letter} = {label}**"]
        if run:
            parts.append(f"run `{run}`")
        if version:
            parts.append(f"(`{version}`)")
        return " · ".join(parts)

    header = ["## Gitaly backend comparison"]
    if experiment:
        header.append(f"**Experiment:** `{experiment}`")
    header.append(side("A", label_a, run_a, version_a))
    header.append(side("B", label_b, run_b, version_b))

    note = (
        "> `A` typically serves repositories from "
        "a local disk; `B` from network object storage (MVCC/elastic). Δ is "
        "`(B - A) / A`, i.e. the B side measured against the A baseline, and the "
        f"**{label_b} better?** column judges each metric in its good direction."
    )

    sections = [
        "\n".join(header),
        note,
        "### Summary",
        render_compare_summary_table(metrics_a, metrics_b,
                                     overall_a, overall_b, label_a, label_b),
    ]

    rpc = render_compare_rpc_latency_table(per_rpc_a, per_rpc_b,
                                           label_a, label_b)
    if rpc:
        sections.append(rpc)

    checks = render_compare_checks_table(
        summary_a.get("root_group", {}), summary_b.get("root_group", {}),
        label_a, label_b)
    if checks:
        sections.append(checks)

    if grafana:
        sections.append(grafana)

    sections.append(
        "_Single-run comparison; numbers reflect one k6 execution per backend "
        "and may have run-to-run variance. Run multiple interleaved rounds for "
        "a stable delta. Latency and throughput are only comparable where both "
        "sides' check pass rates are high and similar; failed RPCs return fast "
        "and skew both metrics._"
    )
    return "\n\n".join(sections) + "\n"


def render_metrics_table(metrics: dict, latency: dict) -> str:
    """Aggregate latency + iteration metrics across all scenarios."""
    iters = metrics.get("iterations", {})
    checks = metrics.get("checks", {})
    dropped = metrics.get("dropped_iterations", {})

    rows = [
        ("Iterations", f"{iters.get('count', 0):,}"),
        ("Throughput", fmt_rate(iters.get("rate", 0))),
        ("Dropped iterations",
         f"{dropped.get('count', 0):,} ({fmt_rate(dropped.get('rate', 0))})"),
        ("Checks passed",
         fmt_pct(checks.get("passes", 0),
                 checks.get("passes", 0) + checks.get("fails", 0))),
        ("RPC latency p50", fmt_ms(latency.get("p50", 0))),
        ("RPC latency p90", fmt_ms(latency.get("p90", 0))),
        ("RPC latency p95", fmt_ms(latency.get("p95", 0))),
        ("RPC latency max", fmt_ms(latency.get("max", 0))),
        ("RPC latency avg", fmt_ms(latency.get("avg", 0))),
    ]

    lines = ["| Metric | Value |", "| --- | --- |"]
    lines.extend(f"| {name} | {value} |" for name, value in rows)
    return "\n".join(lines)


def render_rpc_latency_table(per_rpc: dict) -> str:
    """Per-RPC latency breakdown."""
    if not per_rpc:
        return ""

    lines = ["### Per-RPC latency", "",
             "| RPC | p50 | p90 | p95 | max | avg |",
             "| --- | ---: | ---: | ---: | ---: | ---: |"]
    for rpc, stats in sorted(per_rpc.items()):
        lines.append(
            f"| {rpc} | {fmt_ms(stats.get('p50', 0))} "
            f"| {fmt_ms(stats.get('p90', 0))} | {fmt_ms(stats.get('p95', 0))} "
            f"| {fmt_ms(stats.get('max', 0))} | {fmt_ms(stats.get('avg', 0))} |")
    return "\n".join(lines)


def render_checks_table(root_group: dict) -> str:
    """Per-RPC pass/fail breakdown from k6 ``check()`` calls."""
    checks = root_group.get("checks", {})
    if not checks:
        return "_No per-RPC checks recorded._"

    lines = ["| Check | Passes | Fails | Pass rate |",
             "| --- | ---: | ---: | ---: |"]
    for name, data in sorted(checks.items()):
        passes = data.get("passes", 0)
        fails = data.get("fails", 0)
        lines.append(f"| {name} | {passes:,} | {fails:,} "
                     f"| {fmt_pct(passes, passes + fails)} |")
    return "\n".join(lines)


# Grafana dashboard the benchmark panels live on; <uid>/<slug>, overridable.
_GRAFANA_DASHBOARD = "efneoyur0u4u8f/gitaly-benchmark-testing"


class GrafanaLink:
    """Builds the dashboard deep link and the port-forward command to reach it."""

    def __init__(self, base_url: str, dashboard: str, ns: str, service: str,
                 local_port: int, service_port: int) -> None:
        self.base_url = base_url.rstrip("/")
        self.dashboard = dashboard
        self.ns = ns
        self.service = service
        self.local_port = local_port
        self.service_port = service_port

    def url(self, start_s: float | None, end_s: float | None,
            pad_s: int = 60) -> str:
        # Pad the window so the run's annotation edges stay visible; fall
        # back to the last hour when the window is unknown.
        if start_s and end_s:
            span = (f"&from={int((start_s - pad_s) * 1000)}"
                    f"&to={int((end_s + pad_s) * 1000)}")
        else:
            span = "&from=now-1h&to=now"
        return f"{self.base_url}/d/{self.dashboard}?orgId=1{span}"

    def port_forward_cmd(self) -> str:
        return (f"kubectl -n {self.ns} port-forward "
                f"svc/{self.service} {self.local_port}:{self.service_port}")


def _grafana_link(args) -> GrafanaLink:
    return GrafanaLink(args.grafana_url, args.grafana_dashboard,
                       args.grafana_namespace, args.grafana_service,
                       args.grafana_local_port, args.grafana_service_port)


def read_run_window(run_dir: Path) -> tuple[float | None, float | None]:
    """(start, end) epoch seconds from a run's grafana-run.env, else (None, None)."""
    env_file = run_dir / "grafana-run.env"
    if not env_file.is_file():
        return None, None
    values = dict(ln.split("=", 1) for ln in env_file.read_text().splitlines()
                  if "=" in ln)

    def num(key: str) -> float | None:
        try:
            return float(values[key].strip())
        except (KeyError, ValueError):
            return None

    return num("RUN_START_EPOCH"), num("RUN_END_EPOCH")


def _default_screenshot(explicit: Path | None, results_dir: Path) -> Path | None:
    if explicit:
        return explicit
    candidate = results_dir / "grafana" / "dashboard.png"
    return candidate if candidate.is_file() else None


def render_grafana_section(link: GrafanaLink,
                           windows: list[tuple[str, float | None, float | None]],
                           screenshot: Path | None, results_dir: Path,
                           base_url: str | None) -> str:
    """'View in Grafana' section: a link per run window (plus a combined one),
    the port-forward command, and a grafana screenshot."""
    lines = ["### View in Grafana", ""]
    # Embed the snapshot only with an absolute URL; a relative image path
    # would render as a dead image in an MR note.
    if screenshot and screenshot.is_file() and base_url:
        rel = screenshot.relative_to(results_dir) \
            if screenshot.is_relative_to(results_dir) else screenshot.name
        lines += [f"![Grafana benchmark dashboard]({base_url}/{rel})", ""]
    lines += [
        "Each run is marked with a purple **Benchmark runs** region annotation, "
        "and every k6 section within it with a cyan **k6 sections** region. "
        "Hovering a cyan region shows the section name and the check it "
        "drives, named as in the check tables above (e.g. "
        "`refUpdateBatched UpdateReferences` or `pushSmallCommit-ssh`), "
        "so the panels can be read against the exact window each phase "
        "occupied. Port-forward Grafana, then open a link below:",
        "", "```sh", link.port_forward_cmd(), "```", "",
    ]
    for label, start_s, end_s in windows:
        lines.append(f"- **{label}**: [open dashboard]({link.url(start_s, end_s)})")
    spans = [(s, e) for _, s, e in windows if s and e]
    if len(spans) > 1:
        combined = link.url(min(s for s, _ in spans), max(e for _, e in spans))
        lines.append(f"- **Both runs**: [open dashboard]({combined})")
    return "\n".join(lines).rstrip() + "\n"


def render_plots_section(results_dir: Path, base_url: str | None) -> str:
    """List PNG plots from the experiment's plot.py output."""
    plots_dir = results_dir / "plots"
    if not plots_dir.is_dir():
        return ""

    pngs = sorted(plots_dir.glob("*.png"))
    if not pngs:
        return ""

    lines = ["### Plots", ""]
    for png in pngs:
        rel = png.relative_to(results_dir)
        if base_url:
            url = f"{base_url}/{rel}"
            lines.append(f"![{png.stem}]({url})")
        else:
            lines.append(f"- `{rel}`")
        lines.append("")
    return "\n".join(lines)


def render_report(summary: dict, results_dir: Path, base_url: str | None,
                  experiment: str | None, run_name: str | None,
                  gitaly_version: str | None,
                  overall: dict, per_rpc: dict,
                  job_url: str | None = None,
                  grafana: str | None = None) -> str:
    metrics = summary.get("metrics", {})
    root_group = summary.get("root_group", {})

    header_parts = ["## Gitaly benchmark results"]
    if experiment:
        header_parts.append(f"**Experiment:** `{experiment}`")
    if run_name:
        header_parts.append(f"**Run:** `{run_name}`")
    if gitaly_version:
        # The deployed Gitaly is usually not the MR's code; say what ran.
        header_parts.append(f"**Gitaly under test:** `{gitaly_version}`")

    sections = [
        "\n".join(header_parts),
        "### Summary",
        render_metrics_table(metrics, overall),
    ]

    rpc_latency = render_rpc_latency_table(per_rpc)
    if rpc_latency:
        sections.append(rpc_latency)

    sections.extend([
        "### Per-RPC checks",
        render_checks_table(root_group),
    ])

    plots = render_plots_section(results_dir, base_url)
    if plots:
        sections.append(plots)

    if grafana:
        sections.append(grafana)

    sections.append(
        "_Single-run results; no baseline comparison available yet. "
        "Numbers reflect one k6 execution and may have run-to-run variance._"
    )

    if job_url:
        sections.append(
            f"[View CI job]({job_url}) \u00b7 "
            f"[Browse artifacts]({job_url}/artifacts/browse)")

    return "\n\n".join(sections) + "\n"


def _run_compare(args) -> int:
    if not args.summary.is_file():
        print(f"error: summary A not found: {args.summary}", file=sys.stderr)
        return 1
    if not args.compare.is_file():
        print(f"error: summary B not found: {args.compare}", file=sys.stderr)
        return 1
    # Labels can't be inferred (both backends run the same image), and a
    # wrong default would mislabel every column and verdict.
    if not args.label_a or not args.label_b:
        print("error: --label-a and --label-b are required with --compare",
              file=sys.stderr)
        return 1

    with args.summary.open() as f:
        summary_a = json.load(f)
    with args.compare.open() as f:
        summary_b = json.load(f)

    run_a_dir, run_b_dir = args.summary.parent, args.compare.parent
    grafana = None
    if not args.no_grafana:
        a_start = args.a_start or args.run_start
        a_end = args.a_end or args.run_end
        if not (a_start and a_end):
            a_start, a_end = read_run_window(run_a_dir)
        b_start, b_end = args.b_start, args.b_end
        if not (b_start and b_end):
            b_start, b_end = read_run_window(run_b_dir)
        results_dir = run_a_dir.parent
        base_url = args.artifact_base_url.rstrip("/") \
            if args.artifact_base_url else None
        grafana = render_grafana_section(
            _grafana_link(args),
            [(f"A · {args.label_a} · `{run_a_dir.name}`", a_start, a_end),
             (f"B · {args.label_b} · `{run_b_dir.name}`", b_start, b_end)],
            _default_screenshot(args.grafana_screenshot, results_dir),
            results_dir, base_url)

    markdown = render_compare_report(
        summary_a=summary_a,
        summary_b=summary_b,
        latency_a=agrind_latency(run_a_dir),
        latency_b=agrind_latency(run_b_dir),
        label_a=args.label_a,
        label_b=args.label_b,
        experiment=args.experiment,
        version_a=args.gitaly_version,
        version_b=args.gitaly_version_b,
        run_a=run_a_dir.name,
        run_b=run_b_dir.name,
        grafana=grafana,
    )

    # Default output: comparisons/<A-run>_<B-run>.md next to the run dirs;
    # explicit --output wins.
    if args.output:
        out_path = args.output
    else:
        results_dir = args.summary.parent.parent
        out_path = (results_dir / "comparisons" /
                    f"{args.summary.parent.name}_{args.compare.parent.name}.md")
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(markdown)
    print(f"wrote comparison report to {out_path}", file=sys.stderr)
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Render a Markdown report from a k6 benchmark run.")
    parser.add_argument(
        "summary",
        type=Path,
        help="Path to k6-summary.json (the file produced by --summary-export)")
    parser.add_argument(
        "--output", "-o",
        type=Path,
        help="Write Markdown here instead of stdout")
    parser.add_argument(
        "--artifact-base-url",
        help="Base URL for plot/pprof links. Typically "
             "${CI_JOB_URL}/artifacts/raw/<path-to-run-dir>")
    parser.add_argument(
        "--job-url",
        default=os.environ.get("CI_JOB_URL"),
        help="CI job URL; adds job and artifacts links at the bottom of "
             "the report. Defaults to $CI_JOB_URL.")
    parser.add_argument(
        "--experiment",
        default=os.environ.get("EXPERIMENT"),
        help="Experiment name (for the report header). Defaults to $EXPERIMENT.")
    parser.add_argument(
        "--run-name",
        default=os.environ.get("RUN_NAME"),
        help="Run identifier (for the report header). Defaults to $RUN_NAME.")
    parser.add_argument(
        "--metrics-output",
        type=Path,
        help="Also write an OpenMetrics file here for GitLab's metrics report.")
    parser.add_argument(
        "--gitaly-version",
        default=os.environ.get("GITALY_VERSION"),
        help="Deployed Gitaly image/version under test (for provenance). "
             "Defaults to $GITALY_VERSION, then gitaly-version.txt in the "
             "results dir if present.")
    parser.add_argument(
        "--compare",
        type=Path,
        help="Second k6-summary.json (B side); renders an A/B comparison "
             "report instead.")
    parser.add_argument(
        "--label-a",
        help="Backend name for the A side (the positional summary). Required "
             "with --compare.")
    parser.add_argument(
        "--label-b",
        help="Backend name for the B side (the --compare summary). Required "
             "with --compare.")
    parser.add_argument(
        "--gitaly-version-b",
        help="Gitaly image/version of the B side (for provenance).")

    grp = parser.add_argument_group("Grafana dashboard link")
    grp.add_argument("--no-grafana", action="store_true",
                     help="Omit the 'View in Grafana' section.")
    grp.add_argument("--grafana-url",
                     default=os.environ.get("GRAFANA_URL", "http://localhost:3000"),
                     help="Base URL opened after port-forwarding. "
                          "Default $GRAFANA_URL, else http://localhost:3000.")
    grp.add_argument("--grafana-dashboard", default=_GRAFANA_DASHBOARD,
                     help="Dashboard <uid>/<slug> for the deep link.")
    grp.add_argument("--grafana-namespace",
                     default=os.environ.get("GRAFANA_NAMESPACE", "prometheus"),
                     help="Grafana namespace (for the port-forward command).")
    grp.add_argument("--grafana-service",
                     default=os.environ.get("GRAFANA_SERVICE", "prometheus-grafana"),
                     help="Grafana Service name (for the port-forward command).")
    grp.add_argument("--grafana-local-port", type=int, default=3000)
    grp.add_argument("--grafana-service-port", type=int, default=80)
    grp.add_argument("--grafana-screenshot", type=Path,
                     help="PNG to embed. Default grafana/dashboard.png in the "
                          "results dir if present.")
    grp.add_argument("--run-start", type=float, help="Single-run start (epoch s).")
    grp.add_argument("--run-end", type=float, help="Single-run end (epoch s).")
    grp.add_argument("--a-start", type=float, help="A-run start (epoch s).")
    grp.add_argument("--a-end", type=float, help="A-run end (epoch s).")
    grp.add_argument("--b-start", type=float, help="B-run start (epoch s).")
    grp.add_argument("--b-end", type=float, help="B-run end (epoch s).")

    args = parser.parse_args()

    if not args.summary.is_file():
        print(f"error: summary file not found: {args.summary}", file=sys.stderr)
        return 1

    if args.compare is not None:
        return _run_compare(args)

    with args.summary.open() as f:
        summary = json.load(f)

    results_dir = args.summary.parent
    gitaly_version = args.gitaly_version
    if not gitaly_version:
        version_file = results_dir / "gitaly-version.txt"
        if version_file.is_file():
            gitaly_version = version_file.read_text().strip()
    gitaly_version = gitaly_version or None

    overall, per_rpc = agrind_latency(results_dir)
    base_url = args.artifact_base_url.rstrip("/") if args.artifact_base_url else None

    grafana = None
    if not args.no_grafana:
        run_start, run_end = args.run_start, args.run_end
        if not (run_start and run_end):
            run_start, run_end = read_run_window(results_dir)
        label = f"`{args.run_name}`" if args.run_name else "This run"
        grafana = render_grafana_section(
            _grafana_link(args), [(label, run_start, run_end)],
            _default_screenshot(args.grafana_screenshot, results_dir),
            results_dir, base_url)

    markdown = render_report(
        summary=summary,
        results_dir=results_dir,
        base_url=base_url,
        experiment=args.experiment,
        run_name=args.run_name,
        gitaly_version=gitaly_version,
        overall=overall,
        per_rpc=per_rpc,
        job_url=args.job_url.rstrip("/") if args.job_url else None,
        grafana=grafana,
    )

    if args.output:
        args.output.write_text(markdown)
    else:
        sys.stdout.write(markdown)

    if args.metrics_output:
        args.metrics_output.write_text(
            render_openmetrics(summary.get("metrics", {}),
                               summary.get("root_group", {}),
                               overall, per_rpc, args.experiment,
                               gitaly_version))

    return 0


if __name__ == "__main__":
    sys.exit(main())
