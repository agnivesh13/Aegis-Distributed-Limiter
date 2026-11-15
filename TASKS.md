# Task tracker

Every task from [PLAN.md](PLAN.md), who owns it, and whether it is done. PLAN.md says what to build; this file only tracks progress.

**Current position:** Fable and Opus have both finished every task that does not wait on someone else, through Phase 4, and the cluster runs end to end in Docker. What is left waits on your three pieces, on real benchmark runs, or on reviews. Fable reviewed the Opus work and Opus has fixed every finding (R1 to R13). The bench runner now refuses runs that measure a stub instead of a design. Next up: your three pieces (token bucket, `sliding_window.lua`, `Merge`), then the full scenario runs.

## How to use this file

- Tick a task in the same commit that finishes it, and update "Current position" above.
- A phase is finished only when every "Done when" check under it is ticked.
- Work on one phase at a time, in the order below.

## Who does what

| Owner | Takes |
| --- | --- |
| **You** | The three pieces to defend in interviews, written by hand: the in-memory token bucket, the sliding-window Lua script, the gossip counter merge |
| **Fable** | Work where a subtle bug silently corrupts results: the limiter contract and its tests, concurrency, gossip, the measurement code. Also reviews the hand-written pieces |
| **Opus** | Work the plan already pins down: scaffolding, wiring, Docker, metrics, tooling |

Order inside each phase:

1. Fable writes that phase's tests and contracts.
2. You write your piece, if the phase has one.
3. Opus builds the rest and gets the "Done when" checks passing.
4. Fable reviews the whole phase.
5. Whichever model built the most writes `docs/notes/phase-N.md`.

Switch models with `/model fable` or `/model opus`.

## Phase 0: Warm-up and scaffolding

**Tasks**

- [ ] **You:** A Tour of Go, including the concurrency section
- [ ] **You:** worker pool with goroutines and channels, cancelled through `context`
- [ ] **You:** shared counter written three ways (unprotected, `sync.Mutex`, `sync/atomic`), each run with `go test -race`
- [ ] **You:** HTTP server with one logging middleware and a table-driven test
- [x] **Fable:** `go mod init` (done early; Phase 1 needed it)
- [x] **Fable:** `.gitignore` and `.gitattributes` (done early; Opus extends `.gitignore` as build outputs appear)
- [x] **Opus:** the rest of the directory tree from "Repo layout"
- [x] **Opus:** `README.md` stub with the build, test and run commands
- [x] **Opus:** `cmd/upstream`: echo server on `:8081` with a `-delay` flag and `/healthz`
- [x] **Opus:** multi-stage `Dockerfile`
- [x] **Opus:** `deploy/docker-compose.yml` with only `upstream`
- [x] **Opus:** GitHub Actions workflow running `go vet ./...` and `go test -race ./...`

**Done when**

- [x] `go build ./...` and `go vet ./...` succeed
- [x] `docker compose -f deploy/docker-compose.yml up upstream` answers `curl localhost:8081/hello`
- [ ] Your unprotected counter fails the race detector and the other two pass

## Phase 1: Single-node gateway

**Tasks**

- [x] **Fable:** `internal/limiter/limiter.go`: the contract, plus `SystemClock`
- [x] **Fable:** `internal/limiter/limitertest`: `FakeClock` and the conformance suite `Run(t, factory)`
- [x] **Fable:** `internal/limiter/memory`: sliding window, 64 shards by FNV-1a, janitor, `Close`
- [ ] **You:** the token bucket in `internal/limiter/memory/token_bucket.go` (start by dropping the last argument of `limitertest.Run` in `memory_test.go`)
- [ ] **Fable:** review of your token bucket
- [x] **Opus:** `internal/config`: YAML loading, environment overrides, validation
- [x] **Opus:** `internal/proxy`: reverse proxy behind the middleware chain (recover, request ID, metrics, route match, client identity, rate limit)
- [x] **Opus:** `internal/metrics`: the collectors from the spec, served on the admin port
- [x] **Opus:** `cmd/gateway`: wiring, graceful shutdown with a 10 s drain
- [x] **Opus:** `cmd/loadgen` v0: `-targets`, `-rate`, `-duration`, `-key`; open-loop sender; prints counts and p50, p95, p99
- [x] **Opus:** Compose: add `gateway` with the memory backend
- [x] **Opus:** `BenchmarkAllow`; output saved to `bench/results/phase1-microbench.txt` (sliding window only: re-run once your token bucket exists, the benchmark skips it until then)
- [ ] **Fable:** review of the whole phase (first pass done 2026-11-15, see "Review follow-ups"; the final pass waits on your piece)
- [ ] `docs/notes/phase-1.md`

**Done when**

- [x] `go test -race ./...` passes (in a Linux container; see Environment notes)
- [x] `curl` against the gateway shows the rate-limit headers, then `429` with `Retry-After` once the limit is exceeded
- [x] Load generator at 200 req/s against a 100 req/s sliding-window limit for 30 s: the admitted total is within 2% of 3,000
- [x] `/metrics` shows the request counters and the decision-latency histogram moving

## Phase 2: Redis + Lua, exact and shared

**Tasks**

- [ ] **You:** `internal/limiter/redis/scripts/sliding_window.lua`
- [x] **Fable:** `token_bucket.lua`, checked against the conformance suite
- [ ] **Fable:** review of your `sliding_window.lua` (waits on you)
- [x] **Fable:** `internal/limiter/redis`: go-redis client, embedded scripts, the four integers mapped to `Decision`, per-call deadline
- [x] **Fable:** naive test-only backend (`GET`, compute in Go, `SET`) that shows the lost-update race
- [x] **Fable:** integration tests (build tag `integration`, Redis from testcontainers-go): conformance suite, exactness with three instances, the naive backend over-admitting, Redis outage and recovery
- [x] **Opus:** `fail_open` and `fail_closed` in the rate-limit middleware
- [x] **Opus:** `/readyz` reports not ready while Redis is unreachable
- [x] **Opus:** Compose: add `redis`, make `gateway` scalable, add `toxiproxy` under the `faults` profile
- [x] **Opus:** load generator resolves the `gateway` DNS name to all replicas and round-robins
- [ ] **Fable:** review of the whole phase (first pass done 2026-11-15, see "Review follow-ups"; the final pass waits on your piece)
- [ ] `docs/notes/phase-2.md`

**Done when**

- [x] `go test -race -tags integration ./...` passes
- [ ] 3 gateways, 200 req/s offered against a 100 req/s limit, 30 s: the admitted total is inside the same 2% band as Phase 1. Waits on `sliding_window.lua`; the token bucket route already admits exactly the ideal 319 across 3 gateways (`bench/results/phase2-redis-cluster.txt`)
- [x] Redis stopped for 5 s under load: `fail_open` proxies every request and the error counter rises; `fail_closed` returns `503`; both recover unaided

## Phase 3: Gossip, approximate and Redis-free

**Tasks**

- [ ] **You:** the counter merge in `gossip/store.go` ("Merge (on receiving an entry)" in the plan)
- [x] **Fable:** the rest of `gossip/store.go`: state and decision, fake clock, no network
- [x] **Fable:** `gossip/codec.go`: encode, decode, splitting into packets of at most `max_packet_bytes`; fuzz test
- [x] **Fable:** `gossip/transport.go`: the `Send` / `Peers` interface (done early; `node.go` needed it), and an in-process fake in `sim_test.go`
- [x] **Opus:** the memberlist implementation of `Transport`, wired to the `Node` as its delegate
- [x] **Fable:** `gossip/node.go`: gossip loop, memberlist delegate methods, metrics, `Close` that leaves the cluster
- [x] **Fable:** merge-property tests and the simulation tests (fanout 4, fanout 1 with full sync, 30% loss, restart)
- [ ] **Fable:** review of your merge (waits on you; 13 tests skip themselves until `Merge` exists)
- [x] **Opus:** admin endpoint `/debug/limiter?key=...`
- [x] **Opus:** admin endpoint `/debug/gossip/block?peer=...`, only when `DEBUG_FAULTS=1`
- [x] **Opus:** Compose: run with `LIMITER_BACKEND=gossip`, joining through the `gateway` DNS name
- [ ] **Fable:** review of the whole phase (first pass done 2026-11-15, see "Review follow-ups"; the final pass waits on your piece)
- [ ] `docs/notes/phase-3.md`

**Done when**

- [x] All tests pass under `-race` (the merge and simulation tests skip until `Merge` exists)
- [ ] 3 gateways without Redis: within one second of the load stopping, `/debug/limiter` shows identical totals on all three
- [ ] Under steady 2x load the admitted total exceeds the limit by a measurable margin, and the margin shrinks when `GOSSIP_INTERVAL` drops from 500 ms to 10 ms

## Phase 4: Measure

**Tasks**

- [x] **Fable:** load generator send scheduling in `internal/loadgen`: open loop, late-send counting (more than 5 ms after the scheduled time). `cmd/loadgen` calls `loadgen.Run`
- [x] **Opus:** `cmd/loadgen` v1 flags: `-keys`, `-dist`, `-shape`, per-gateway weights
- [x] **Opus:** CSV output: `seq, key, send_unix_ns, latency_ns, status, gateway_id`
- [x] **Fable:** `cmd/analyze` and `internal/analyze`: replay through the reference limiter; one row per run in `bench/results/summary.csv`
- [x] **Opus:** `cmd/bench`: start the cluster for a scenario, wait for `/readyz`, run the load, inject faults, scrape `/metrics`, tear down
- [x] **Opus:** `bench/scenarios.yaml`: the scenario matrix
- [x] **Opus:** `bench/plot.py`: the two charts from `summary.csv`
- [ ] **Opus:** run every scenario 3 times. S0 is done (3 reps, in `summary.csv`). The rest wait on `sliding_window.lua` (every Redis scenario) and `Merge` (every gossip scenario)
- [x] **Fable:** check the noise floor (S0): passes, at most 0.034% against the 1% rule (details in the review)
- [ ] **Fable:** review of the whole phase (first pass done 2026-11-15, see "Review follow-ups"; the final pass waits on your piece)
- [ ] `docs/notes/phase-4.md`

**Done when**

- [ ] `go run ./cmd/bench -scenario S1` produces a summary row end to end with no manual steps
- [x] S0 shows over-admission under 1%
- [ ] `bench/results/` holds `summary.csv` and both charts

## Phase 5: Write-up and polish

**Tasks**

- [ ] **Opus:** `README.md`: pitch, architecture diagram, quick start, results table, both charts, limitations (a stub with build, test and run commands exists; the rest needs results)
- [ ] **Opus:** `docs/report.md` draft: question, setup, method, results per scenario, limitations (waits on results)
- [ ] **Fable:** the prediction-against-measurement analysis in `docs/report.md`
- [ ] **Fable:** check that every number in the README and report traces to a file in `bench/results/`
- [x] **Opus:** Grafana dashboard
- [ ] **You:** 2-minute screen recording
- [ ] **You:** resume bullets and a 60-second spoken summary, built on measured numbers
- [ ] **You:** tag `v1.0`

**Done when**

- [ ] Someone who has never seen the repo can clone it, run one command, and reproduce one row of the results table
- [ ] Every number in the README and the report traces to a file in `bench/results/`

## Review follow-ups

From Fable's review of the gateway, cluster and bench runner: [docs/reviews/2026-11-15-gateway-cluster-bench.md](docs/reviews/2026-11-15-gateway-cluster-bench.md). Each item there says where the problem is, how to fix it and how to test the fix.

**Before running the scenario matrix**

- [x] **Opus:** R1: a non-fault run with limiter errors must fail, not append a row to `summary.csv`
- [x] **Opus:** R1 follow-on: a multi-gateway gossip run in which no node merged anything is refused too (`gossip_entries_merged_total`); found while fixing R1
- [x] **Opus:** R2: snapshot and subtract per gateway, so a killed gateway (F1) cannot make the window negative
- [x] **Opus:** R3: pass every `config.EnvNames()` variable through Compose; reject unknown scenario `env` keys
- [x] **Opus:** R4: S9 on the memory backend runs with one gateway
- [x] **Opus:** R5: `-start-at` flag in `cmd/loadgen`, used by `cmd/bench` to time faults and the warm-up snapshot

**Soon**

- [x] **Opus:** R6: with `trust_forwarded_for`, use the last `X-Forwarded-For` entry, not the first
- [x] **Opus:** R7: reject API keys longer than 128 bytes with `400`
- [x] **Opus:** R8: correct the help text of `gateway_upstream_seconds`
- [x] **Opus:** R9: a cancelled client request is not a limiter error
- [x] **Opus:** R10: one percentile function, shared by the load generator and the analyzer

**Small**

- [x] **Opus:** R11: reject an explicit `cost: 0`
- [x] **Opus:** R12: `cmd/bench` checks for a running dev cluster on the same host ports and says so
- [x] **Opus:** R13: README says the Grafana login is for local use only

## Open questions

Carried over from PLAN.md; answer them when they start blocking a task.

- [ ] Which two measured numbers headline the resume bullet?
- [x] Custom load generator from day one, or vegeta first and custom in Phase 4? **Answered: custom from day one.** It is built on the open-loop scheduler and writes the CSV the analyzer reads; vegeta remains an option as a cross-check.
- [ ] Own SWIM implementation: worth it, or leave in the backlog?

## Environment notes

- **Go is not on the shell PATH** on this machine. It is installed at `C:\Program Files\Go\bin`; add that to PATH or call it by full path.
- **The race detector cannot build on Windows here:** the only C compiler is 32-bit MinGW. Run it in a container instead, with Docker Desktop started:

  ```
  docker run --rm -v "<repo path>:/src:ro" -w /src golang:1.27 sh -c "go vet ./... && go test -race ./..."
  ```
