# Phase 1: single-node gateway

One gateway enforces limits from memory, and a test suite defines the behaviour every later backend must match. The long explanations are in [learnings/](../../learnings/README.md), notes 03 to 06, 11, 12 and 16.

## What was built

| Piece | Where | By |
| --- | --- | --- |
| Limiter contract, `SystemClock`, shared sliding-window arithmetic | `internal/limiter` | Fable |
| Fake clock and conformance suite | `internal/limiter/limitertest` | Fable |
| Memory backend: both algorithms, 64 shards, janitor | `internal/limiter/memory` | Fable |
| Config, proxy chain, metrics, admin port | `internal/config`, `proxy`, `metrics`, `admin` | Opus |
| Gateway and upstream binaries, load generator | `cmd/gateway`, `cmd/upstream`, `cmd/loadgen` | Opus |
| Image, Compose, CI | `Dockerfile`, `deploy/`, `.github/` | Opus |

## Design choices

- **The clock is injected into every backend.** Tests move time instantly, and the analyzer later replays recorded runs through the same limiter.
- **One conformance suite for all backends**, taking a factory. A backend lists the algorithms it supports; anything unlisted must be refused, and the suite checks that too.
- **Durations may be up to 1 ms above the exact value, never below.** Redis works in whole milliseconds and rounds up; rounding down would break the promise `RetryAfter` makes.
- **A rejected request writes nothing** (sliding window) or takes nothing (token bucket). The sliding window decides on copies of the stored counts to guarantee it.
- **64 shards, each padded to one cache line.** Requests for different keys rarely share a lock, and locking one shard does not slow its neighbours.
- **The sliding-window arithmetic lives in one function**, `limiter.SlidingWindowDecision`, used by memory and gossip and mirrored by the Lua script.
- **The token bucket refills lazily:** no timers, two numbers per key.
- **`cost` above capacity is an error, not a wait:** such a request can never succeed.
- **Config is strict:** unknown keys are errors, and every invalid value is reported at once.
- **The chain order is fixed:** recover, request ID, metrics, route match, client identity, rate limit.
- **`X-Forwarded-For` is off by default,** and when on, only the entry the trusted proxy appended is used.

## What each test proves

| Test | Proves |
| --- | --- |
| `limitertest.Run` on memory, 29 cases | Both algorithms follow the plan's "exact behaviour" sections; details per case in learnings/06 |
| `ConcurrentAdmitsExactlyCapacity` | 50 goroutines, 10,000 requests, frozen clock: exactly the capacity is admitted, under the race detector |
| `TestSweptKeyDecidesLikeUnsweptKey` | Removing an expired key changes no decision |
| `TestKeysSpreadOverAllShards` | The hash spreads keys evenly over the shards |
| `internal/proxy` tests | Headers, 429 with `Retry-After`, 404, 502, both error policies, panics becoming 500, the limiter deadline, client identity, an oversized API key, a client that hangs up |
| `internal/config` tests | Defaults, environment overrides, strictness, and every validation message |

The suite itself was tested by injecting seven bugs; each was caught.

## Done when

| Check | Result |
| --- | --- |
| `go test -race ./...` | Passes (in a Linux container; the Windows host lacks a 64-bit C compiler) |
| Headers, then `429` with `Retry-After` | Seen with `curl`; a 300-request burst admitted exactly 100 |
| 200 req/s against 100 req/s for 30 s within 2% of 3,000 | 2,971 admitted; the reference limiter's ideal for the same requests was also 2,971 |
| `/metrics` moving | Request counters and the decision histogram both advance |

Microbenchmarks: `bench/results/phase1-microbench.txt`. About 120 ns per decision with no allocations; four cores give 2.4× the throughput over 10,000 keys and none on a single key.
