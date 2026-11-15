# Phase 4: measure

Every scenario in the plan's experiment design runs from one command and produces rows of results. The long explanations are in [learnings/](../../learnings/README.md), notes 10, 13, 15 and 18.

## What was built

| Piece | Where | By |
| --- | --- | --- |
| Open-loop send schedule, late-send counting | `internal/loadgen` | Fable |
| Analyzer: replay through the reference limiter, summary rows | `internal/analyze`, `cmd/analyze` | Fable |
| Load generator: key distributions, burst shape, weights, CSV, `-start-at` | `cmd/loadgen` | Opus |
| Bench runner: cluster, readiness, load, faults, snapshots, validity checks, `-missing`, `-only`, `-table` | `cmd/bench` | Opus, then Fable |
| Scenario matrix, chart script | `bench/scenarios.yaml`, `bench/plot.py` | Opus |
| The runs themselves | `bench/results/` | Fable |

## Design choices

- **Open loop.** Sends follow a fixed schedule computed from the sequence number; a slow server cannot slow the load down.
- **Accuracy by replay.** The ideal count comes from replaying the run's own requests through the single-node memory limiter, warm-up included, counting only the measured window.
- **One start instant** is passed to the load generator and used for faults and metric snapshots.
- **Metric windows are subtracted per gateway,** so a killed gateway cannot make them negative.
- **A run that measured a broken setup is refused,** not recorded: nearly every request failing in the limiter, or gossip nodes that merged nothing from each other.
- **Limiter errors below that level are a result.** Redis missing its 50 ms deadline on some requests under heavy load is what the scenario is there to show.
- **The memory backend always runs on one gateway.** Several would each enforce the full limit.

## Changes from the plan's matrix

| Scenario | Plan | Run as | Why |
| --- | --- | --- | --- |
| S3 top step | 10× the limit | 6× | At 10,000 req/s, 1.8% of sends left late on the test laptop; the plan's sizing rule is 1% |
| S7 burst | 10× for 1 s | 4× for 1 s | From cold, 22% to 45% of a 10,000 req/s burst left late |
| S9 memory | 3 gateways | 1 gateway | Three memory gateways share nothing |

The six rows recorded at the plan's sizes are kept in `bench/results/summary-oversized.csv`.

## Results

132 runs. Full table with medians and ranges: [bench/results/RESULTS.md](../../bench/results/RESULTS.md). Charts: `bench/results/chart-over-admission-vs-interval.png`, `bench/results/chart-decision-latency-vs-redis-delay.png`.

**Head to head (S1: 3 gateways, 2× load, gossip every 100 ms, median of 3):**

| Backend | Over-admission | p99 decision latency |
| --- | --- | --- |
| Redis + Lua | 0.00% | 395 µs |
| Gossip | 4.45% (3.71 to 4.61) | 12 µs |

### The plan's hypotheses

| Hypothesis | Verdict | Evidence |
| --- | --- | --- |
| Redis stays exact at any node count | Confirmed | S2: 0.00% at 1, 3 and 5 gateways |
| Redis decision latency tracks the network round trip | Confirmed | S6: p99 0.74 ms, 2.9 ms and 6.6 ms at 0, 1 and 5 ms of added delay |
| Gossip decision latency stays near the in-memory baseline | Confirmed | 10 to 12 µs in every scenario, against 10 µs for memory (both at the histogram's first bucket) |
| Gossip over-admission grows with interval | Confirmed | S4, 3 gateways: 0.54%, 1.99%, 3.86%, 20.1% at 10, 50, 100, 500 ms |
| ... with node count | Confirmed | S2: 0.00%, 3.74%, 7.07% at 1, 3, 5 gateways |
| ... with burstiness | Confirmed, weakly | S7: 4.81% for a 4× burst from idle, with a peak of 1.45× the limit in one second |
| Below the limit, both backends behave the same | Confirmed | S3 at 0.5×: 0.00% over-admission and 0.00% false rejects for both |

### The first-order prediction

The plan predicts over-admission of about `((N − 1) / N) × lag / W`, with lag about half the gossip interval.

| Gateways | Interval | Predicted | Measured |
| --- | --- | --- | --- |
| 3 | 10 ms | 0.33% | 0.54% |
| 3 | 50 ms | 1.67% | 1.99% |
| 3 | 100 ms | 3.33% | 3.86% |
| 3 | 500 ms | 16.7% | 20.1% |
| 5 | 100 ms | 4.0% | 6.30% |
| 5 | 500 ms | 20.0% | 32.1% |

At 3 gateways and intervals of 50 ms and above, the model is within about a fifth of the measurement, and always below it, which fits a lag slightly longer than half the interval (delivery time is left out). At 10 ms it is further off in relative terms (0.33% against 0.54%): a fixed delivery time of a few milliseconds is a large part of so short an interval. At 5 gateways it is well under. The model assumes each round reaches every peer; with fanout 2 of 4 peers it reaches half, so news takes longer. S5 shows exactly that at 5 gateways and 100 ms: fanout 4 gives 4.97% (prediction 4.0%), fanout 2 gives 6.86%, fanout 1 gives 12.8%.

### Faults

| Scenario | What happened |
| --- | --- |
| F1, gateway killed | Requests to the dead gateway failed at the client (27%). The survivors kept limiting: Redis stayed at 0.00% over; gossip was at 1.57% (S1, with all three alive throughout, was 4.45%) |
| F2, Redis down 5 s | `fail_open`: 30.6% over-admission, no client errors. `fail_closed`: 0% over-admission, 27.7% of requests answered 503 |
| F3, partition 2 + 2 for 10 s | 33.4% over-admission and a peak of 2.15× the limit: each half enforced the full limit alone, as the design predicts |

### Throughput ceiling (S9, single runs)

Client p99 stays near 2 ms at 2,000 req/s for all three backends. By 8,000 req/s it is 28 to 50 ms, and Redis decision p99 has risen from 0.6 ms to 7.1 ms. At 16,000 req/s every backend collapses (75% to 95% errors). On this laptop the ceiling is the machine, shared by the load generator and the gateways, between 8,000 and 16,000 req/s; gossip decisions stay at 10 µs right up to it.

## Limits of these numbers

- Everything ran on one laptop under Docker, with the load generator and gateways competing for CPU. Relative differences are meaningful; absolute numbers are not.
- Decision latencies come from a histogram whose buckets grow by 1.5×, so a p99 is known only to within its bucket. Memory and gossip sit in the first bucket (under 10 µs).
- The sliding window counter itself allows about 1.05× the limit in some one-second spans, on any backend, including one exact node (S0). Peak figures should be read against that.
- F1 had 1.3% to 9.9% late sends, caused by requests piling up against the dead gateway, and one of the three S7 Redis runs had 6.7%. Their accuracy figures are less reliable than the others.
- S9 and the two phase-check runs are single runs, not medians of three.

## What each test proves

Tests for this phase's code are described in learnings/10 (schedule and analyzer), learnings/13 (runner and scenario file) and learnings/15 (validity rules, per-gateway windows).

## Done when

| Check | Result |
| --- | --- |
| `go run ./cmd/bench -scenario S1` produces a summary row end to end | Yes: six rows, no manual steps |
| S0 shows over-admission under 1% | 0.00% to 0.01%; false rejects at most 0.03% |
| `bench/results/` holds `summary.csv` and both charts | Yes |
