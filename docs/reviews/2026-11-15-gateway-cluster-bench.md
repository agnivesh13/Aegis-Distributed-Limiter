# Review: gateway, cluster and bench runner

Reviewed 2026-11-15 by Fable. Covers the Opus work in commits `32fb056`, `a2bf079` and `3cca03a`: `internal/config`, `internal/proxy`, `internal/metrics`, `internal/admin`, `cmd/gateway`, `cmd/upstream`, `cmd/loadgen`, `cmd/bench`, `deploy/` and `bench/`.

**Verdict:** the gateway is sound and its checks are real. The bench runner has five problems that would put wrong numbers into `summary.csv`; fix those (R1 to R5) before running the scenario matrix. The rest can follow.

Each finding says where it is, what goes wrong, how to fix it and how to test the fix. They are also listed as tasks in [TASKS.md](../../TASKS.md).

## Fix before running the scenarios

### R1. A run on a backend that cannot decide is recorded as a result

- **Where:** `cmd/bench/main.go`, `runJob`.
- **What goes wrong:** if the backend returns errors for every request (today: any Redis scenario, because `sliding_window.lua` is a stub), `fail_open` lets everything through and the runner appends a normal-looking row showing roughly 100% over-admission. Nothing marks the row as invalid. The start-up log line added to the gateway does not help here, because the runner never reads gateway logs.
- **Fix:** the runner already has `window.LimiterErrors`. For a run with no fault, treat any limiter error as a failed run: print why and do not append a row. For fault runs (F2) errors are the point, so keep the row.
- **Test:** extract the decision into a small function, `validRun(fault Fault, limiterErrors float64) error`, and unit-test both cases.

### R2. Killing a gateway makes the measured window negative

- **Where:** `cmd/bench/main.go` (`scrape`, `runJob`) and `cmd/bench/prom.go` (`Sub`).
- **What goes wrong:** `scrape` sums counters over every gateway it can reach. In F1 the "before" snapshot has all gateways and the "after" snapshot is missing the killed one, so `after − before` goes negative. Reproduced during this review: three gateways before, two after, gave buckets of `-100` and `-200` and a p99 of `0`.
- **Fix:** keep one snapshot per gateway, keyed by its admin address. Subtract gateway by gateway, and leave out any gateway missing from either scrape. Sum the differences.
- **Test:** the reproduction above, turned around: three gateways before, two after, expect the window to equal the two survivors' differences and no negative count.

### R3. Environment variables the Compose file does not list are silently dropped

- **Where:** `deploy/docker-compose.yml`, `gateway.environment`; `cmd/bench/scenario.go`.
- **What goes wrong:** the config reads eleven variables. Compose passes six of them to the gateway. A scenario that sets `GOSSIP_RETRANSMIT`, `GOSSIP_FULL_SYNC_INTERVAL`, `GOSSIP_JOIN` or `GATEWAY_UPSTREAM` would run with the default and record a row labelled with the value it did not use. No current scenario does this, so nothing is wrong yet; the first new scenario that does will be.
- **Fix:** pass every variable in `config.EnvNames()` through Compose. In `Load`, reject a scenario `env` key that is not in that list.
- **Test:** a test in `internal/config` or `cmd/bench` that reads the Compose file and fails if any name from `config.EnvNames()` is missing from it.

### R4. S9 on the memory backend is not one limit

- **Where:** `bench/scenarios.yaml`, scenario S9.
- **What goes wrong:** S9 runs memory, Redis and gossip at the default three gateways. Three memory gateways share nothing, so each enforces the full limit and the row will show about 200% over-admission. S9 is about throughput, so that figure is meaningless there, but it will sit in `summary.csv` beside real ones.
- **Fix:** give the memory backend one gateway in S9 (the "1 node baseline" the plan describes), and say in the scenario comment that S9's accuracy columns are not the result.
- **Test:** extend `TestScenarioFileLoads`: no job may pair the memory backend with more than one gateway.

### R5. Faults and the warm-up snapshot are timed from the wrong moment

- **Where:** `cmd/bench/main.go`, `runJob`: `start := time.Now()` is taken before `docker compose run loadgen`.
- **What goes wrong:** the load generator's container takes a second or two to start, so the load begins after `start`. A fault "at 10 s" lands 8 to 9 s into the load, and the "end of warm-up" scrape is early by the same amount. The analyzer is unaffected (it works from the CSV), but F1 to F3 timings in the report would be off, and the decision-latency window would include a little warm-up.
- **Fix:** add a `-start-at <unix nanoseconds>` flag to `cmd/loadgen` that sets `Schedule.Start`; the scheduler already sleeps until each send's time. The runner passes "now plus 5 s" and uses that same instant for faults and the snapshot.
- **Test:** in `cmd/loadgen`, run with `-start-at` 300 ms ahead and check the first CSV row's send time equals it exactly.

## Fix soon

### R6. With `trust_forwarded_for`, the client still chooses its own identity

- **Where:** `internal/proxy/proxy.go`, `ClientID`.
- **What goes wrong:** the code takes the *first* entry of `X-Forwarded-For`. A load balancer *appends* the address it saw to whatever the client sent. So a client that sends `X-Forwarded-For: 1.2.3.4` arrives as `1.2.3.4, <real address>`, and the gateway limits `1.2.3.4`. Changing that value on every request avoids the limit entirely. The switch is off by default, which is why this is not in the first group.
- **Fix:** take the *last* entry, which is the one the trusted proxy wrote. (With several trusted proxies in a row the rule is "count back that many"; one hop is enough here. Say so in the config comment.)
- **Test:** `ClientID` with `X-Forwarded-For: 9.9.9.9, 8.8.8.8` and trust on must return `8.8.8.8`. Update the existing table test, which currently expects `9.9.9.9`.

### R7. The API key has no length limit

- **Where:** `internal/proxy/proxy.go`, `ClientID` and `rateLimit`.
- **What goes wrong:** the key goes straight into the limiter key, so into the memory backend's map, a Redis key name and gossip packets. A client can send a 100 KB key on every request: memory grows, and a gossip entry no longer fits in a packet.
- **Fix:** answer `400` for a key longer than, say, 128 bytes.
- **Test:** a 129-byte key gets `400` and the limiter is never asked.

### R8. `gateway_upstream_seconds` does not measure what its help text says

- **Where:** `internal/metrics/metrics.go` (help text) and `internal/proxy/proxy.go` (the `upstream` handler).
- **What goes wrong:** the help says "until its response headers". The timer wraps `rp.ServeHTTP`, which returns only after the whole body has been copied to the client, so it includes body transfer and a slow client.
- **Fix:** the simple one is to correct the help text: "Time to proxy a request to the upstream and relay its whole response."
- **Test:** none needed for a text change.

### R9. A client hanging up is counted as a limiter failure

- **Where:** `internal/proxy/proxy.go`, `rateLimit`.
- **What goes wrong:** the limiter's context comes from the request's. If the client disconnects while Redis is being asked, the call fails with `context.Canceled`, `gateway_limiter_errors_total` goes up and the error policy is applied to a client that is gone. Under load with client timeouts this makes Redis look unhealthy.
- **Fix:** when `r.Context().Err()` is non-nil after the call, return without counting an error or writing a response.
- **Test:** a stub limiter that blocks until its context ends; cancel the request; expect the error counter to stay at 0.

### R10. The load generator and the analyzer compute percentiles differently

- **Where:** `cmd/loadgen/main.go`, `summarise`.
- **What goes wrong:** `summarise` has its own percentile with a `+0.999999` rounding trick; `internal/analyze` has `percentile` using `math.Ceil`. They agree on most inputs and can differ by one rank on others, so the two tools can print different p99s for the same run.
- **Fix:** export `analyze.Percentile` and call it from the load generator.
- **Test:** covered by the existing `TestPercentile` once there is a single implementation.

## Small

- **R11. `cost: 0` is accepted.** `Parse` turns 0 into 1, so an explicit `cost: 0` in the file passes instead of being rejected. Make the field a pointer (the scenario file's `warmup` shows the pattern) so "left out" and "zero" differ.
- **R12. The bench project and a running dev cluster fight over host ports.** Both publish 8080 to 8084. If a dev cluster is up, `cmd/bench` fails inside `docker compose up` with a long error. Check first and print one clear line.
- **R13. Grafana ships with `admin`/`admin` and anonymous viewing.** Fine on a laptop; the README should say it is for local use only.

## What is good, and should stay as it is

- **Config is strict:** unknown keys are errors and every problem is reported at once. This already paid off in testing.
- **Each middleware is one small function, and the chain order is written once**, with tests that check behaviour through the whole chain, not layer by layer in isolation.
- **Health and readiness are separate**, and readiness turns false before shutdown begins.
- **The start-up probe** turned a silent misconfiguration into the first line of the log.
- **Checks were run in the real cluster and their outputs saved.** The token-bucket figure of 319 was checked against a hand calculation, which is the right instinct.
- **The load generator decides every random choice before the run**, so runs are repeatable and the send loop stays cheap.

## Status of Fable's own tasks

- **S0 noise floor: checked, passes.** From `bench/results/summary.csv`: over-admission 0.000%, 0.000%, 0.010%; false rejects 0.034%, 0.017%, 0.000%; late sends at most 11 of 70,000. All far under the plan's 1%. The residual is what the plan predicted: send time stands in for arrival time, so a handful of requests are decided in a slightly different order than the replay assumes.
- **The 1.05× peak in S0 is the algorithm, not the measurement.** It appears on a single exact node. Report it as the sliding window counter's own approximation; it is a floor shared by all three backends.
- **Phase reviews: first pass done** (this document, for the Opus parts of Phases 0 to 4). The final pass on each phase waits for the hand-written piece in it.
- **Still waiting:** reviews of the token bucket, `sliding_window.lua` and `Merge`; the prediction-against-measurement analysis, which needs S4.
