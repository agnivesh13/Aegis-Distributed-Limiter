"""Draw the report's two charts from bench/results/summary.csv.

    python bench/plot.py [summary.csv] [output folder]

1. Over-admission against gossip interval, one line per node count (S4).
2. p99 decision latency per backend against injected Redis delay (S6),
   with the gossip and memory backends from S1 and S0 as reference lines.

Each point is the median of a scenario's repetitions; the bars show the
lowest and highest repetition. Only matplotlib is needed.
"""

import csv
import statistics
import sys
from collections import defaultdict
from pathlib import Path

import matplotlib

matplotlib.use("Agg")  # draw to files; no window needed
import matplotlib.pyplot as plt  # noqa: E402


def read_rows(path):
    with open(path, newline="", encoding="utf-8") as f:
        rows = list(csv.DictReader(f))
    for row in rows:
        # The note column holds key=value pairs written by cmd/bench.
        row["meta"] = dict(p.split("=", 1) for p in row.get("note", "").split() if "=" in p)
    return rows


def duration_ms(text):
    """'10ms' -> 10.0, '1s' -> 1000.0, '0s' -> 0.0."""
    for unit, scale in (("ms", 1.0), ("us", 0.001), ("µs", 0.001), ("s", 1000.0)):
        if text.endswith(unit):
            return float(text[: -len(unit)]) * scale
    return float(text)


def median_and_range(values):
    return statistics.median(values), min(values), max(values)


def errorbar(ax, xs, groups, label, **style):
    """groups maps x -> list of values; draws medians with min-max bars."""
    xs = sorted(x for x in xs if groups[x])
    if not xs:
        return False
    med, low, high = zip(*(median_and_range(groups[x]) for x in xs))
    yerr = [[m - lo for m, lo in zip(med, low)], [hi - m for m, hi in zip(med, high)]]
    ax.errorbar(xs, med, yerr=yerr, marker="o", capsize=4, label=label, **style)
    return True


def chart_over_admission(rows, out):
    by_nodes = defaultdict(lambda: defaultdict(list))
    for r in rows:
        if r["scenario"] == "S4" and r["backend"] == "gossip" and r["meta"].get("interval"):
            by_nodes[int(r["gateways"])][duration_ms(r["meta"]["interval"])].append(float(r["over_admission_pct"]))
    if not by_nodes:
        print("chart 1 skipped: no S4 rows in the summary yet")
        return
    fig, ax = plt.subplots(figsize=(7, 4.5))
    for nodes in sorted(by_nodes):
        groups = by_nodes[nodes]
        errorbar(ax, groups.keys(), groups, f"{nodes} gateways")
    ax.set_xscale("log")
    ax.set_xlabel("gossip interval (ms, log scale)")
    ax.set_ylabel("over-admission (% of ideal)")
    ax.set_title("Gossip limiter: over-admission against gossip interval")
    ax.grid(True, which="both", alpha=0.3)
    ax.legend()
    fig.tight_layout()
    path = out / "chart-over-admission-vs-interval.png"
    fig.savefig(path, dpi=150)
    print("wrote", path)


def chart_decision_latency(rows, out):
    redis = defaultdict(list)
    refs = defaultdict(list)
    for r in rows:
        p99 = r["meta"].get("decision_p99_us")
        if p99 is None:
            continue
        if r["scenario"] == "S6" and r["backend"] == "redis" and "redis_delay" in r["meta"]:
            redis[duration_ms(r["meta"]["redis_delay"])].append(float(p99) / 1000)
        elif (r["scenario"], r["backend"]) in (("S1", "gossip"), ("S0", "memory")):
            refs[r["backend"]].append(float(p99) / 1000)
    if not redis:
        print("chart 2 skipped: no S6 rows in the summary yet")
        return
    fig, ax = plt.subplots(figsize=(7, 4.5))
    errorbar(ax, redis.keys(), redis, "redis (through toxiproxy)")
    for backend, style in (("gossip", "--"), ("memory", ":")):
        if refs[backend]:
            ax.axhline(statistics.median(refs[backend]), linestyle=style, color="grey",
                       label=f"{backend} (no network call)")
    ax.set_xlabel("delay added to every Redis reply (ms)")
    ax.set_ylabel("p99 decision latency (ms)")
    ax.set_title("Decision latency against Redis network delay")
    ax.grid(True, alpha=0.3)
    ax.legend()
    fig.tight_layout()
    path = out / "chart-decision-latency-vs-redis-delay.png"
    fig.savefig(path, dpi=150)
    print("wrote", path)


def main():
    summary = Path(sys.argv[1] if len(sys.argv) > 1 else "bench/results/summary.csv")
    out = Path(sys.argv[2] if len(sys.argv) > 2 else summary.parent)
    out.mkdir(parents=True, exist_ok=True)
    rows = read_rows(summary)
    chart_over_admission(rows, out)
    chart_decision_latency(rows, out)


if __name__ == "__main__":
    main()
