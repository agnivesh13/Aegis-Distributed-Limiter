# 06. The conformance suite: how the tests are designed

Files: [internal/limiter/limitertest/clock.go](../internal/limiter/limitertest/clock.go), [internal/limiter/limitertest/suite.go](../internal/limiter/limitertest/suite.go)

## What a conformance suite is

Most tests check one piece of code. A **conformance suite** checks a *promise*: it is written against the interface, and any implementation can be plugged into it.

This project will have three backends that must behave the same. Writing three sets of tests would let them drift apart. So there is one set, and each backend runs it:

```go
func newBackend(_ *testing.T, clock limiter.Clock) limiter.Limiter {
	return memory.New(clock)
}

func TestConformance(t *testing.T) {
	limitertest.Run(t, newBackend, limiter.SlidingWindow)
}
```

`newBackend` is a **factory**: a function that builds a fresh limiter. The suite calls it at the start of every case, so no case can be affected by leftovers from another. The Redis backend will pass its own factory to the very same `Run`.

The last argument lists what the backend supports. Anything not listed *must* be refused with `ErrUnsupported`, and the suite checks that too. So the gossip backend (sliding window only) cannot quietly half-implement the token bucket.

## The fake clock

```go
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *FakeClock) Now() time.Time          { ... return c.now }
func (c *FakeClock) Advance(d time.Duration) { ... c.now = c.now.Add(d) }
```

A clock that stands still until told to move. The mutex is there because one test fires 50 goroutines at the limiter, and they all read the clock at once.

Every case starts its clock at the same fixed moment:

```go
var epoch = time.Unix(1_700_000_000, 0)
```

This number is not random. It is an exact multiple of one second, so each case starts **exactly on a window boundary**. A test can then say "advance 250 ms" and know it is precisely a quarter of the way through a window.

## How a case reads

```go
t.Run("FullLimitAfterTwoIdleWindows", func(t *testing.T) {
	h := newHarness(t, newLimiter)
	h.clock.Advance(250 * time.Millisecond)
	h.exactly(key, l, 1, 100)
	h.clock.Advance(2 * time.Second)
	h.exactly(key, l, 1, 100)
})
```

Almost plain English: use up the limit, wait two windows, the full limit is back.

The helper `exactly(key, limit, cost, n)` means: the next `n` requests must be allowed **and the one after must be rejected**. The second half matters. A test that only checked "100 are allowed" would pass for a limiter that allows everything.

There is also a check that runs on *every single decision* in the suite, inside `allow`:

- `Limit` equals the configured rate.
- `Remaining` is never negative.
- An allowed request has `RetryAfter` of zero.
- A rejected request has a `RetryAfter` above zero.

These are **invariants**: things that must be true always, not just in one scenario.

## Problem 1: computers cannot store 0.1

Run this in any language:

```text
0.1 + 0.2  →  0.30000000000000004
```

A `float64` stores numbers in binary. One tenth in binary is like one third in decimal: it goes on for ever (0.333...), so it gets cut off and stored as a number very slightly different from 0.1. Usually nobody notices. In a rate limiter it shows up at exact boundaries.

Suppose a test said: drain the bucket, wait 99 ms, wait 1 ms more, expect exactly one token. Inside, the limiter adds `0.99 + 0.01`. If that comes out as `0.9999999999999999`, then `floor` gives 0 tokens and the request is rejected. The test fails, yet the limiter has no real bug.

**How the suite avoids it:**

- **Never add up fractions and then demand an exact answer.** The refill test uses two keys. One is checked at 99 ms (must reject). The other is untouched until 100 ms, so its refill is one single step, `100 ms × 10 per second = 1`, which is exact.
- **Pick numbers that binary can store exactly.** Halves, quarters and eighths are exact in binary, the way tenths are exact in decimal. The test that checks `RetryAfter` to the millisecond uses a limit of **8**, so every fraction is some number of eighths:

  ```go
  // Limit 8 keeps every fraction here exact in binary floating
  // point, so the 1 ms boundaries below are safe on any backend.
  l8 := limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 8, Period: time.Second}
  ```

The lesson for your own tests: when a test fails by a hair at a boundary, ask whether the *test's numbers* are the problem before changing the code.

## Problem 2: Redis counts in milliseconds

The memory backend works in nanoseconds. The Redis scripts work in whole milliseconds and round up. Ask both "how long until I can retry?" and one may say `10 ms`, the other `10.000001 ms` or `11 ms`. Both are right.

A test demanding an exact value would fail one of them. So durations are compared with a one-sided tolerance:

```go
func near(t *testing.T, name string, got, want time.Duration) {
	if got < want || got > want+tolerance {   // tolerance = 1 ms
		t.Errorf(...)
	}
}
```

**Up to 1 ms above the exact value is accepted. Anything below is a failure.** The direction is the point:

- Too long by a millisecond: the client waits a little more than needed. Harmless.
- Too short: the client waits as told, comes back, and is rejected again. That breaks the promise `RetryAfter` makes.

And the suite does not only check the number. It checks the promise itself:

```go
h.clock.Advance(124 * time.Millisecond)
if h.allow(key, l8, 1).Allowed {            // 1 ms early: must still be rejected
	t.Error("allowed 1 ms before RetryAfter elapsed")
}
h.clock.Advance(time.Millisecond)
if !h.allow(key, l8, 1).Allowed {           // on time: must be allowed
	t.Error("rejected after waiting RetryAfter")
}
```

Testing what a value *means* is stronger than testing what it *equals*.

## The concurrency test

```go
const goroutines, perGoroutine = 50, 200 // 10,000 requests, clock frozen
```

Fifty goroutines send 200 requests each at the same key, with the clock frozen so no refill can happen. The limit is 100. Exactly 100 must be admitted: not 99, not 101.

If the lock were missing, this is where the lost-update bug from [05-memory-backend.md](05-memory-backend.md) would show up as 103 or 107 admitted. Run with `-race`, Go also watches the memory accesses directly and reports any unprotected one, even if the count happened to come out right that time.

## Who tests the tests?

A test suite can be wrong in two ways: it can demand something impossible, or it can let bugs through. I checked for both.

**Is every case possible to pass?** The token bucket cases had no implementation to run against, because the token bucket is yours to write. I wrote a quick throwaway one, ran the suite, saw all 29 cases pass, and deleted it. Now you know that a failing token bucket test means your code needs work, not that the test is broken.

**Does the suite catch bugs?** I broke the code on purpose, one bug at a time, and ran the suite. This is called **mutation testing**.

| Bug put in | Cases that failed |
| --- | --- |
| Sliding window: `<=` changed to `<` (off by one) | 15 |
| Sliding window: previous window ignored | 6 |
| Sliding window: `RetryAfter` rounded down | 6 |
| Sliding window: `Remaining` off by one | 14 |
| Token bucket: no cap at capacity | 2 |
| Token bucket: negative elapsed time not clamped | 2 |
| Token bucket: a rejected request takes tokens | 4 |

Every bug was caught. If one had slipped through with zero failures, that would have exposed a hole in the suite to fill with a new case.

You can do this yourself. Change a `<=` to `<` in `sliding_window.go`, run `go test ./...`, read which cases fail and why, then undo the change. It is one of the fastest ways to learn what each test is really checking.

## The cases, and what each proves

**Token bucket** (10 tokens per second, capacity 20)

| Case | What it proves |
| --- | --- |
| `FullBucketAllowsCapacity` | A new key starts full: 20 allowed, the 21st rejected, with correct `Remaining` all the way down |
| `BurstZeroMeansRate` | `Burst: 0` falls back to `Rate` as the capacity |
| `RefillsOneTokenPerInterval` | One token returns after exactly 100 ms, and not after 99 ms |
| `NeverExceedsCapacity` | An hour of idleness still leaves only 20 tokens |
| `CostConsumesTokens` | A cost of 5 takes 5 tokens |
| `RejectedConsumesNothing` | A rejected request takes nothing |
| `RetryAfterIsTimeUntilEnoughTokens` | Waiting `RetryAfter` is enough; waiting 1 ms less is not |
| `ClockSteppingBackAddsNoTokens` | A clock that jumps backwards does not break the bucket |

**Sliding window** (100 per second)

| Case | What it proves |
| --- | --- |
| `AllowsLimitInOneWindow` | Exactly 100 in one window; the wait runs to the end of the window |
| `StartOfNextWindow`, `QuarterThrough...`, `HalfThrough...`, `ThreeQuartersThrough...` | The previous window's weight falls in a straight line: 0, 25, 50, 75 allowed |
| `FullLimitAfterTwoIdleWindows` | Old counts are forgotten after two windows |
| `RetryAfterWaitsForPreviousWindowToDecay` | The decay formula for `RetryAfter` is right to the millisecond |
| `CostConsumesCount`, `RejectedConsumesNothing` | Cost is charged when allowed and never when rejected |

**Both algorithms**

| Case | What it proves |
| --- | --- |
| `KeysAreIndependent` | One client using up its limit does not affect another |
| `InvalidCostChangesNothing` | Cost 0, negative, or above capacity is refused and nothing is charged |
| `ResetAfterRestoresFullLimit` | Waiting `ResetAfter` really does give the full limit back |
| `ConcurrentAdmitsExactlyCapacity` | No lost updates under 50 goroutines |

## Check yourself

- What is the difference between a normal unit test and a conformance suite?
- Why does `exactly` check that the request *after* the last allowed one is rejected?
- Why does the tolerance on durations go only upwards?
- Why does one test use a limit of 8 where the others use 100?
- What does mutation testing tell you that a green test run does not?
