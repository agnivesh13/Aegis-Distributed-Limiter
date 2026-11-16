# 01. What was built so far

This is the story of the work in order. Each step says what was done, why, and what problem came up. The problems are explained fully in [07-problems-and-fixes.md](07-problems-and-fixes.md).

## The project in one paragraph

A rate limiter decides whether a request is allowed or must be rejected because the client has sent too many. This project puts a rate limiter inside an HTTP gateway, runs several copies of the gateway, and asks: how do the copies share their counts? One answer is exact but slower (every request asks Redis). The other is fast but approximate (gateways gossip their counts to each other). The project builds both and measures the difference.

Before any of that can exist, there has to be a limiter that works on one machine. That is where the work started.

## Step 0: Installing a skill and splitting the work

| What | Why |
| --- | --- |
| Installed the `improve-codebase-architecture` skill with `npx skills add` | You asked for it. It lives in `.agents/` and is linked into `.claude/skills/` |
| Read the whole plan and split every task between you, Fable and Opus | Two models are available. The harder model does the work where a subtle bug would silently corrupt results; the other does the well-specified wiring |

The split is in [TASKS.md](../TASKS.md). The rule behind it: you write the three pieces you must defend in an interview, Fable takes correctness-critical work and reviews, Opus takes scaffolding.

## Step 1: Looking at the machine before writing code

Before writing anything I checked what tools existed. This found three surprises:

- `go` was "not recognized", although Go was installed.
- Docker was installed but not running.
- The only C compiler was a 32-bit one, which later broke the race detector.

**Lesson:** check the tools first. Finding a broken tool after writing 500 lines is much worse than finding it before writing one.

## Step 2: Creating the Go module

```
go mod init github.com/agnivesh13/ratelimit-gateway
```

This creates `go.mod`, the file that makes a folder a Go project. Scaffolding was Opus's job in the split, but nothing in Go compiles without a module, so this one command had to come first.

I also made a branch, `phase-1-limiter-core`, so `main` stays untouched until the work is reviewed.

## Step 3: The limiter contract

File: [internal/limiter/limiter.go](../internal/limiter/limiter.go)

The plan fixes the exact types every backend must use, so this step was mostly copying them in carefully. I added two small things the plan did not list:

- `SystemClock`, the real clock (the plan asked for it in the Phase 1 tasks).
- `Limit.Capacity()`, a helper that answers "what is the largest cost one request may have?". All three backends need that same answer, so it belongs in one place.

Explained in [03-limiter-contract.md](03-limiter-contract.md).

## Step 4: The tests, before the code

Files: [clock.go](../internal/limiter/limitertest/clock.go), [suite.go](../internal/limiter/limitertest/suite.go)

The plan's rule 3 says: write limiter tests before limiter code. So the next thing written was the *conformance suite*: one set of tests that any backend can be plugged into. The memory backend runs it now. Redis and gossip will run the very same tests later, which is how we know all three behave alike.

The hard part here was arithmetic, not Go. Computers cannot store numbers like 0.1 exactly, so a test that says "after exactly 100 ms one token is back" can fail for reasons that have nothing to do with a bug. Every number in the suite was chosen so the arithmetic comes out exact. Explained in [06-conformance-suite.md](06-conformance-suite.md).

## Step 5: The memory backend

Files: [memory.go](../internal/limiter/memory/memory.go), [sliding_window.go](../internal/limiter/memory/sliding_window.go), [token_bucket.go](../internal/limiter/memory/token_bucket.go)

Three parts:

- **The sliding window algorithm.** See [04-sliding-window.md](04-sliding-window.md).
- **The storage around it:** 64 locked maps, a background cleaner and a clean shutdown. See [05-memory-backend.md](05-memory-backend.md).
- **A stub for the token bucket.** The plan reserves the token bucket for you, so the file only holds an empty function that says "not supported yet", with notes on what to build.

## Step 6: Proving the tests are good

Passing tests prove little if the tests are weak. Two extra checks:

1. **Are the token bucket tests even possible to pass?** I could not know without an implementation, and the implementation is yours to write. So I wrote a throwaway one, ran the suite (all 29 cases passed), and deleted it. It is not in the repo.
2. **Do the tests catch bugs?** I put seven bugs into the code, one at a time, and checked the suite failed each time. It did, every time.

## Step 7: The race detector

`go test -race` watches a running program for two goroutines touching the same memory without a lock. The plan requires it before every commit.

It would not build on Windows here. The fix was to run the tests inside a Linux container with Docker. It passed.

## Step 8: Housekeeping

| Change | Why |
| --- | --- |
| Module path changed to `github.com/agnivesh13/Aegis-Distributed-Limiter` | The first path did not match the real GitHub repo, so links to packages would not resolve |
| Plan renamed to `PLAN.md` | The plan itself asks to be saved under that name |
| `.gitattributes` added | Git on Windows was about to rewrite line endings, which `gofmt` reports as badly formatted files |
| `.gitignore` added | Keeps local tool folders and build output out of Git |
| `TASKS.md` added | One place to see every task, its owner and whether it is done |

## Step 9: The Redis backend (Phase 2)

Files: [internal/limiter/redis/](../internal/limiter/redis/)

A limiter that several gateways can share exactly, by running each decision as a Lua script inside Redis. I wrote the token bucket script, the Go backend around both scripts, and integration tests against a real Redis in Docker. The sliding window script is yours, so that file holds only comments for now.

The headline result: a deliberately naive GET-then-SET backend admitted over 2,000 requests against a limit of 100. The Lua version admitted exactly 100, every time.

Explained in [08-redis-backend.md](08-redis-backend.md).

## Step 10: One copy of the sliding window arithmetic

The gossip backend needed the same sliding window formulas as the memory backend. Two copies of a formula drift apart, so the arithmetic moved into [internal/limiter/window.go](../internal/limiter/window.go) and both backends call it. The existing tests passed unchanged afterwards, which is what a refactor should look like: same behaviour, better structure.

## Step 11: The gossip backend (Phase 3)

Files: [internal/limiter/gossip/](../internal/limiter/gossip/)

A limiter that needs no Redis: each gateway decides from its own memory and tells the others what it admitted in the background. I wrote the store, the wire format with a fuzz test, the node with its gossip loop, and a simulated five-node cluster that runs inside one test. The counter merge is yours; thirteen tests skip themselves until it exists.

Explained in [09-gossip-backend.md](09-gossip-backend.md).

## Step 12: The measuring code (Phase 4)

Files: [internal/loadgen/](../internal/loadgen/), [internal/analyze/](../internal/analyze/), [cmd/analyze/](../cmd/analyze/)

The schedule that decides when each load-test request leaves, and the analyzer that works out how far a run was from an ideal limiter. The analyzer's own tests caught a wrong formula in my first version.

Explained in [10-measuring.md](10-measuring.md).

## Step 13: The gateway itself (Opus)

Files: [internal/config/](../internal/config/), [internal/proxy/](../internal/proxy/), [internal/metrics/](../internal/metrics/), [internal/admin/](../internal/admin/), [cmd/gateway/](../cmd/gateway/), [cmd/upstream/](../cmd/upstream/)

With the limiters in place, the session switched to Opus for the program that uses them: configuration, the middleware chain, metrics, the admin port and graceful shutdown. Explained in [11-the-gateway.md](11-the-gateway.md).

## Step 14: Containers, load and the real checks (Opus)

Files: [Dockerfile](../Dockerfile), [deploy/](../deploy/), [cmd/loadgen/](../cmd/loadgen/), [.github/workflows/ci.yml](../.github/workflows/ci.yml)

One image for every program, a Compose file for the whole cluster, CI, the load generator, and the memberlist transport for gossip. Then every "Done when" check that could run was run in the real cluster. Explained in [12-running-the-cluster.md](12-running-the-cluster.md).

## Step 15: The bench runner and the first scenario (Opus)

Files: [cmd/bench/](../cmd/bench/), [bench/](../bench/)

One command per scenario, the full matrix as data, the chart script and the Grafana dashboard. S0, the noise floor, ran three times and came in under 0.04%. Explained in [13-bench-runner.md](13-bench-runner.md).

## Step 16: Fable reviews the Opus work

File: [docs/reviews/2026-11-15-gateway-cluster-bench.md](../docs/reviews/2026-11-15-gateway-cluster-bench.md)

With the model switched back to Fable, the gateway, cluster and bench runner were reviewed. Thirteen findings, five of which must be fixed before the scenario matrix is run because they would put wrong numbers into the results. The worst was reproduced with a throwaway test. The S0 noise floor was checked and passes. Explained in [14-code-review.md](14-code-review.md).

## Step 17: Opus fixes the review findings

All thirteen findings were fixed, each with a test that fails without the fix. Fixing R1 uncovered a second case of the same bug (gossip runs with an unwritten `Merge`), now refused too. A real S1 run proved both refusals. Explained in [15-review-fixes.md](15-review-fixes.md).

## Step 18: The three core pieces, and the full measurement

You handed your own tasks to Fable. The token bucket, the sliding-window Lua script and the gossip merge were written and checked ([16-the-three-core-pieces.md](16-the-three-core-pieces.md)), and the warm-up exercises were added ([17-warmup-exercises.md](17-warmup-exercises.md)).

With nothing left blocked, the whole scenario matrix ran: 132 runs over about three hours. The headline: Redis is exact at 395 µs per decision; gossip takes 12 µs and lets 4.5% extra through. Three more problems surfaced during the run and were fixed. Explained in [18-reading-the-results.md](18-reading-the-results.md).

## Step 19: The write-up

The README now opens with the result, and `docs/report.md` holds the full method, every scenario and the limits. Writing the report turned up a flaw in the plan's prediction formula; the corrected one matches the 500 ms measurement almost exactly. Every figure in both documents was recomputed from `summary.csv` and compared. Explained in [19-the-write-up-and-the-model.md](19-the-write-up-and-the-model.md).

## What is left

| Task | Who |
| --- | --- |
| A Tour of Go, the screen recording, tagging `v1.0` | You |
| Studying these notes until you can explain the project unaided | You |

## The commits

| Commit | What it holds |
| --- | --- |
| `66a68c7` | The limiter contract |
| `617221c` | The fake clock and the conformance suite |
| `e7a1a62` | The memory backend with the sliding window |
| `347a032` | `.gitattributes`, `.gitignore`, the skills lock file (and, by accident, the plan rename) |
| `ffa5418` | The module path change and `TASKS.md` |
| `9c3f5b5` | The first seven notes in `learnings/` |
| `50cd93e` | The Redis backend, its Lua token bucket and integration tests |
| `42cae19` | The gossip backend: store, codec, node and simulations |
| `44634ca` | The load schedule and the analyzer |
| the commits after `44634ca` | The gateway, the cluster, the bench runner (Opus) |

Each commit builds and passes its tests on its own. Small commits like these mean that if something breaks later, `git bisect` can find the exact commit that broke it.

## What comes next

Three pieces are yours, in this order:

1. The token bucket, in [token_bucket.go](../internal/limiter/memory/token_bucket.go).
2. The sliding window script, in [sliding_window.lua](../internal/limiter/redis/scripts/sliding_window.lua).
3. The counter merge, in [merge.go](../internal/limiter/gossip/merge.go).

Each file holds notes on what to build, and the tests for each already exist. Start by reading [04-sliding-window.md](04-sliding-window.md) and [05-memory-backend.md](05-memory-backend.md): the token bucket has the same shape as the sliding window code, with different arithmetic.

## Check yourself

- Why were the tests written before the limiter code?
- Why was a throwaway token bucket written and then deleted?
- What does injecting a bug on purpose tell you that a passing test does not?
