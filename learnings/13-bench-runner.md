# 13. The bench runner, the scenario matrix and the charts

Files: [cmd/bench/](../cmd/bench/), [bench/scenarios.yaml](../bench/scenarios.yaml), [bench/plot.py](../bench/plot.py), [deploy/gateway-bench.yaml](../deploy/gateway-bench.yaml), [deploy/grafana/dashboards/aegis.json](../deploy/grafana/dashboards/aegis.json)

## Why a runner at all

The experiment plan has 13 scenarios, most with several points, each repeated 3 times: well over a hundred runs. Each run needs a fresh cluster, a wait until it is ready, the load, perhaps a fault at the right moment, a scrape, an analysis and a tear-down. Done by hand, some runs would differ in small ways nobody wrote down, and the comparison would be quietly unfair.

So every step lives in one program:

```
go run ./cmd/bench -scenario S1
```

The plan's test for this phase is exactly that: one command, one summary row, no manual steps.

## The scenario file

[bench/scenarios.yaml](../bench/scenarios.yaml) holds the matrix from the plan. The design rule is **defaults plus overrides**: the file states the defaults once (3 gateways, 2,000 requests per second, 10 keys, 35 s with 5 s warm-up, 3 repetitions) and each run lists only what it changes:

```yaml
  - id: S4
    backends: [gossip]
    runs:
      - { label: i10ms-n3, gateways: 3, env: { GOSSIP_INTERVAL: 10ms } }
```

That run differs from the defaults in exactly two ways, and you can see both on one line. This is the plan's "fix a default for each variable and sweep one at a time", written as data.

**A small trap: zero as a real value.** S7 needs a warm-up of 0 s. If `warmup` were a plain duration, 0 would look the same as "not set", and the default 5 s would be filled in. So the field is a *pointer*: `nil` means "not set, use the default" and a pointer to 0 means "zero, on purpose". The same trick marks S6's `redis_delay: 0ms`, which must still route Redis through toxiproxy.

## One run, step by step

`runJob` in [main.go](../cmd/bench/main.go):

1. **Start a fresh cluster** with the run's environment: `docker compose up -d --wait --scale gateway=N`. For S6 it adds the `faults` profile and points the gateways at toxiproxy.
2. **Find the admin ports** with `docker compose port --index i gateway 9090`, since replicas get whichever ports are free.
3. **Set the Redis delay** through toxiproxy's API, if the run has one.
4. **For gossip, wait until every gateway sees every member.** `/readyz` is not enough: a gateway is ready before it has found its peers, and a run that starts with one gateway still alone would over-admit for reasons that have nothing to do with the gossip interval.
5. **Start the load** and, at the same moment, a goroutine that injects the run's fault on schedule.
6. **Scrape every gateway twice:** when the warm-up ends, and after the load. Subtracting the first from the second leaves only the measured window, matching the analyzer, which also skips the warm-up.
7. **Analyse** the CSV and **append** one row to `bench/results/summary.csv`.
8. **Tear down,** whatever happened, then pause 10 s so the laptop cools down.

The tear-down sits in the caller, not in `runJob`, so it happens even when a run fails half-way. And one failed run is reported and skipped, not fatal: losing an hour of runs to one bad point would be worse.

### The faults

| Fault | How |
| --- | --- |
| Gateway crash (F1) | `docker kill` one replica's container |
| Redis outage (F2) | `docker compose stop redis`, wait, `start redis` |
| Partition (F3) | `POST /debug/gossip/block` between every pair across the two halves; `DELETE` to heal |
| Redis delay (S6) | A toxiproxy "latency" toxic on replies from Redis |

All of them use Docker or the gateway's own endpoints, never host network tools, so the runner works the same on Windows, macOS and Linux (plan rule 8).

### Reading a histogram without a library

The runner needs the p99 decision latency from the gateways' histograms, summed over all replicas. Rather than depend on a parsing library whose API changes between versions, [prom.go](../cmd/bench/prom.go) reads the few lines it needs from the text format, then estimates the quantile the way Prometheus's `histogram_quantile` does:

1. The `+Inf` bucket holds the total count. The target rank is `q × total`.
2. Walk the buckets in order until the running count reaches the rank.
3. Assume observations are spread evenly inside that bucket and interpolate between its lower and upper bound.

The test checks the interpolation with numbers worked out by hand: 100 observations, 50 at or below 1 ms, 90 at or below 2 ms, so the 70th percentile lies halfway between 1 ms and 2 ms: 1.5 ms.

## The bench config

[deploy/gateway-bench.yaml](../deploy/gateway-bench.yaml) has a single sliding-window route. The default config also has a token-bucket route, which the gossip backend refuses. Comparing backends is only fair if everything except the backend is the same, so all experiments use this one file, selected by `GATEWAY_CONFIG`.

## The first real result: S0

S0 asks how noisy the measurement itself is: one gateway, the exact memory backend, so any error reported is the measuring tool's own.

| Rep | Admitted | Ideal | Over-admission | False rejects | Peak in any 1 s |
| --- | --- | --- | --- | --- | --- |
| 1 | 29,635 | 29,645 | 0.00% | 0.03% | 1.05× |
| 2 | 29,632 | 29,637 | 0.00% | 0.02% | 1.05× |
| 3 | 29,634 | 29,631 | 0.01% | 0.00% | 1.05× |

The plan's rule: if S0 is over 1%, fix the measurement before trusting anything else. It is under 0.04% on every repetition. The small differences come from send time standing in for arrival time, which the plan predicted.

**The 1.05 peak is a real finding.** Even the exact single-node limiter let 5% more than the limit through in some true one-second span. That is the sliding window counter's even-spread assumption from [04-sliding-window.md](04-sliding-window.md), now measured. Every backend in the comparison uses the same algorithm, so this 5% is a floor they all share, and it should be stated in the report.

## The charts

[bench/plot.py](../bench/plot.py) reads `summary.csv` and draws the plan's two charts. Each point is the median of the repetitions, with a bar from the lowest to the highest, as the plan's measurement rules ask. Runs record their settings as `key=value` pairs in the `note` column (`interval=10ms fanout=2 decision_p99_us=...`), which is how the script knows where each point goes.

I tested the script on a made-up summary file kept outside the repo, to see both charts draw correctly. Neither real chart can be drawn yet: chart 1 needs S4 (gossip, so your `Merge`) and chart 2 needs S6 (Redis sliding window, so your script). The script says so plainly when the data is missing.

## The Grafana dashboard

[aegis.json](../deploy/grafana/dashboards/aegis.json) is loaded automatically when Grafana starts with the `observability` profile. Its panels: requests per second by decision and by gateway, decision latency p50 and p99 per backend, upstream p99 and limiter errors, tracked keys, gossip members, gossip traffic and gossip delivery age. Every query was checked against a live cluster before it was committed.

## After the three pieces were written

When this note was first written, every Redis and gossip scenario was blocked on pieces not yet written, and only S0 could run. Once they existed, the whole matrix ran: 132 runs. What the numbers mean is in [18-reading-the-results.md](18-reading-the-results.md).

The runner gained three flags on the way:

| Flag | Use |
| --- | --- |
| `-missing` | Run only the points that have no row in the summary yet, to redo failed runs without repeating good ones |
| `-only n5` | Run only the points whose label contains the text |
| `-table` | Print the summary as a table of medians and ranges |

## Check yourself

- Why is a runner better than running each experiment by hand, beyond saving time?
- What does "defaults plus overrides" buy you when reading the scenario file?
- Why is `warmup` a pointer?
- Why must a gossip run wait for full membership, when `/readyz` already says ready?
- Why scrape twice and subtract?
- Walk through estimating a p70 from three buckets.
- What does S0 measure, and what would you do if it came out at 3%?
- What does the 1.05 peak in S0 tell you about the algorithm?
