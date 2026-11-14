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

## The commits

| Commit | What it holds |
| --- | --- |
| `66a68c7` | The limiter contract |
| `617221c` | The fake clock and the conformance suite |
| `e7a1a62` | The memory backend with the sliding window |
| `347a032` | `.gitattributes`, `.gitignore`, the skills lock file (and, by accident, the plan rename) |
| `ffa5418` | The module path change and `TASKS.md` |

Each commit builds and passes its tests on its own. Small commits like these mean that if something breaks later, `git bisect` can find the exact commit that broke it.

## What comes next

Your token bucket, in [token_bucket.go](../internal/limiter/memory/token_bucket.go). The tests for it already exist. Start by reading [04-sliding-window.md](04-sliding-window.md) and [05-memory-backend.md](05-memory-backend.md): the token bucket has the same shape as the sliding window code, with different arithmetic.

## Check yourself

- Why were the tests written before the limiter code?
- Why was a throwaway token bucket written and then deleted?
- What does injecting a bug on purpose tell you that a passing test does not?
