# Phase 5: write-up and polish

The results are written up for two readers: [README.md](../../README.md) for a two-minute visit and [docs/report.md](../report.md) for someone checking the method. The long explanation is in [learnings/19](../../learnings/19-the-write-up-and-the-model.md).

## What was written

| Piece | Where | By |
| --- | --- | --- |
| Pitch, results, charts, architecture, quick start, reproduce steps, limitations | `README.md` | Fable (an Opus task in the tracker; it blocked the two below) |
| Question, setup, method, results per scenario, coordination cost, limitations, next steps | `docs/report.md` | Fable |
| Prediction against measurement | `docs/report.md` | Fable |
| Number trace | Last section of `docs/report.md` | Fable |
| Grafana dashboard | `deploy/grafana/` | Opus, in Phase 4 |
| Resume bullets and spoken summary | `docs/resume.md` | Fable, in Phase 4 |

## Design choices

- **Results first in the README.** Installation comes after the reader knows why to bother.
- **Reproduce without touching committed results.** The README's command writes to a scratch summary under the git-ignored `bench/results/raw/`.
- **The corrected model is reported beside the plan's, not in place of it.** The plan's first-order formula stays in `PLAN.md` as the prediction that was made; the report shows where it holds and adds `x = p / (1 − p)`.
- **Unexplained results are listed as unexplained:** the rise at 6× load, and the lag's share of the interval at 5 gateways.

## What the analysis found

- The first-order model undercounts because peers admit at the actual rate, not at the limit. The corrected form predicts 20.0% at 500 ms and 3 gateways; measured 20.1%.
- The lag implied by the 3-gateway runs is half the interval plus 1 to 6 ms, which is within run-to-run spread.
- At 5 gateways and 100 ms, the implied lag is 59, 80 and 142 ms at fanout 4, 2 and 1.
- The default gossip configuration, measured 12 times across four scenarios, ranges from 3.13% to 4.61%.

## Checks

| Check | Result |
| --- | --- |
| Someone new can run one command and reproduce a row | `go run ./cmd/bench -scenario S0 -reps 1 -summary bench/results/raw/my-summary.csv` was run from this tree on 2026-11-16: 0.00% over-admission, 0.00% false rejects, peak 1.05. Not yet tried from a fresh clone on another machine |
| Every number traces to `bench/results/` | Each figure in both documents was compared with medians recomputed from `summary.csv`; four draft errors were corrected |

## Still open

The screen recording, A Tour of Go and the `v1.0` tag are the owner's.
