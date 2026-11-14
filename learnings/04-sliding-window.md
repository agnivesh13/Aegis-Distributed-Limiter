# 04. The sliding window counter

File: [internal/limiter/memory/sliding_window.go](../internal/limiter/memory/sliding_window.go)

This note builds the algorithm up from the simplest idea, shows where each simpler idea fails, and then walks through the code.

Throughout, the limit is **100 requests per second**.

## Attempt 1: the fixed window

The simplest limiter: cut time into one-second boxes and count requests in the current box.

```text
 window 0          window 1          window 2
|-----------------|-----------------|-----------------|
0s                1s                2s                3s
```

Keep one counter. If it is below 100, allow the request and add one. When a new second starts, reset the counter to zero.

**Where it fails.** A client sends 100 requests at 0.9 s and another 100 at 1.1 s:

```text
 window 0          window 1
|--------------xxx|xxx--------------|
               0.9s 1.1s
```

Each window saw exactly 100, so every request was allowed. But 200 requests went through in 0.2 seconds. The limit was 100 per second and the client got double. This is the **boundary burst**.

## Attempt 2: remember every request

Store the timestamp of every request. To decide, count the timestamps in the last one second.

This is exact. It is also expensive: a client allowed 10,000 requests per second needs 10,000 timestamps stored. Memory grows with traffic. (The plan calls this the "sliding window log".)

## The sliding window counter: two numbers per key

A middle path. Keep only **two counters** per key:

- `cur`: requests in the current window
- `prev`: requests in the previous window

Then *estimate* how many requests fell in the last one second, by assuming the previous window's requests were spread evenly.

Suppose we are 25% of the way through the current window:

```text
        previous window            current window
|-------------------------|-------------------------|
                  |<------ the last 1 second ------>|
                  |   75% of prev   |  all of cur   |
                                           ^
                                          now (25% in)
```

The last one second covers all of the current window so far, plus the last 75% of the previous window. So:

```text
estimate = cur + prev × (1 − elapsed / W)
```

where `W` is the window length and `elapsed` is how far into the current window we are.

The term `(1 − elapsed / W)` is the **weight** of the previous window. It starts at 1 when a window begins and falls steadily to 0 by the end.

## Worked example

The previous window had 100 requests. The current window has had none.

| Time into current window | Weight of prev | Estimate | Still allowed |
| --- | --- | --- | --- |
| 0 ms (just started) | 1.00 | 100 | 0 |
| 250 ms | 0.75 | 75 | 25 |
| 500 ms | 0.50 | 50 | 50 |
| 750 ms | 0.25 | 25 | 75 |

These four rows are exactly four test cases in the conformance suite.

**Now replay the boundary burst.** 100 requests at 0.9 s, then more at 1.1 s. At 1.1 s we are 10% into the new window, so the weight is 0.9 and the estimate is `0 + 100 × 0.9 = 90`. Only 10 more are allowed, where the fixed window allowed 100. The burst is gone.

## Where it is still wrong

The estimate *assumes* the previous window's requests were evenly spread. If they were not, the estimate is off.

Example: all 100 of the previous window's requests arrived in its final 10 ms. Half-way through the current window, the estimate says 50. In truth, all 100 are still inside the last second. The limiter allows 50 more: 150 in a true one-second span.

This is the accepted cost of storing two numbers and not ten thousand. The plan lists it openly: "Approximate: assumes the previous window's requests were evenly spread". Be ready to say this in an interview; knowing where your own algorithm is wrong is worth more than claiming it is perfect.

## The code, step by step

### Step 1: which window are we in?

```go
w := int64(l.Period)        // window length in nanoseconds
nowNs := now.UnixNano()     // nanoseconds since 1970
idx := nowNs / w            // window number
elapsed := nowNs - idx*w    // how far into this window
```

Integer division throws away the remainder, so `nowNs / w` counts whole windows since 1970. That is the **window number**. What is left over is `elapsed`.

With a one-second window, time `1700000000.25 s` gives `idx = 1700000000` and `elapsed = 0.25 s`.

Windows are therefore lined up with the calendar, not with a client's first request. This matters later: every gateway computes the same window number from its own clock, so they agree on window boundaries without talking to each other.

### Step 2: lock, then bring the stored counts up to date

```go
s := b.shardFor(key)
s.mu.Lock()
defer s.mu.Unlock()

st := s.windows[key]
var cur, prev int64
if st != nil && st.period == l.Period {
	switch st.idx {
	case idx:
		cur, prev = st.cur, st.prev
	case idx - 1:
		prev = st.cur
	}
}
```

The stored state says "in window number `st.idx`, there were `st.cur` requests". Time may have moved on since it was written. Three cases:

| Stored window | Meaning | Result |
| --- | --- | --- |
| Same as now (`idx`) | Still in the same window | Use both counts as they are |
| One behind (`idx - 1`) | A new window has started | The old `cur` is now the previous window; the new `cur` is 0 |
| Anything else, or no state | Two or more windows have passed | Everything is too old; both are 0 |

`cur` and `prev` start at zero, so the third case needs no code. That is the zero-value rule doing useful work.

**Notice these are local copies.** The stored state `st` is not changed yet. That is on purpose; see step 4.

### Step 3: estimate and decide

```go
estimate := float64(cur) + float64(prev)*(1-float64(elapsed)/float64(w))

case estimate+c <= limit:    // allow
```

The formula from above, then the test: does the estimate plus this request's cost still fit?

### Step 4: store only when allowed

```go
d.Allowed = true
cur += int64(cost)
estimate += c
if st == nil {
	st = &window{}
	s.windows[key] = st
}
*st = window{period: l.Period, idx: idx, cur: cur, prev: prev, expires: now.Add(2 * l.Period)}
```

Only an allowed request writes anything. The contract says "a rejected request consumes nothing", and the simplest way to guarantee that is to not touch the stored state at all on a reject. This is why step 2 worked on copies.

`*st = window{...}` replaces the whole stored struct in one go, so no field can be left over from an older window.

### Step 5: how long should a rejected client wait?

There are two different reasons a request is rejected, and each has its own waiting time.

**Reason A: the previous window's weight is in the way.**

```go
case float64(cur)+c <= limit:
	wait := float64(w)*(1-(limit-float64(cur)-c)/float64(prev)) - float64(elapsed)
```

The current window alone has room (`cur + cost <= limit`). The request fails only because `prev × weight` is still too large. That weight shrinks as time passes, so we can work out exactly when the request will fit.

We need the time `t` into the window at which:

```text
cur + prev × (1 − t / W) + cost  =  limit
```

Solve for `t`:

```text
prev × (1 − t / W)  =  limit − cur − cost
1 − t / W           =  (limit − cur − cost) / prev
t                   =  W × (1 − (limit − cur − cost) / prev)
```

That is the moment it fits. We are already at `elapsed`, so the wait is `t − elapsed`. That is the line of code above.

*Numbers:* limit 8, `prev = 8`, `cur = 4`, half-way through the window, cost 1.
`t = W × (1 − 3/8) = 0.625 W`. We are at `0.5 W`. Wait = `0.125 W` = 125 ms.
Check: at 625 ms the estimate is `4 + 8 × 0.375 = 7`, and `7 + 1 = 8` fits exactly.

**Reason B: the current window alone is too full.**

```go
default:
	d.RetryAfter = time.Duration(w - elapsed)
```

Here `cur + cost > limit`. No amount of decay in the previous window helps; nothing can fit until this window ends. So the wait is the time left in the window.

This is a **lower bound**, not a promise. When the window ends, today's `cur` becomes tomorrow's `prev` at full weight, so the request may be rejected once more with a short Reason A wait. The test `AllowsLimitInOneWindow` shows this: 750 ms to the end of the window, and then the next test shows a further 10 ms.

**Why the wait is rounded up:**

```go
d.RetryAfter = max(1, time.Duration(math.Ceil(wait)))
```

`math.Ceil` rounds up to the next whole nanosecond. If the limiter rounded down, a client that waited exactly `RetryAfter` could arrive a hair too early and be rejected again. Rounding up means waiting the stated time is always enough. The `max(1, ...)` makes sure a rejected request never reports a zero wait.

### Step 6: `Remaining` and `ResetAfter`

```go
d.Remaining = max(0, int(math.Floor(limit-estimate)))
```

How many whole requests still fit. `math.Floor` rounds down, because 2.7 requests of room is room for 2. `max(0, ...)` keeps it from ever going negative.

```go
switch {
case cur > 0:
	d.ResetAfter = time.Duration(2*w - elapsed)
case prev > 0:
	d.ResetAfter = time.Duration(w - elapsed)
}
```

When is the *full* limit back? Requests in the current window keep some weight until the end of the *next* window, so that is `2W − elapsed` away. If only the previous window has requests, they fade out at the end of this window: `W − elapsed`. If both are empty, the full limit is available now, and `ResetAfter` stays at its zero value.

### The expiry

```go
expires: now.Add(2 * l.Period)
```

After two full windows with no request, both counts would be treated as zero anyway (the third row of the table in step 2). So the stored state is useless from that moment and can be deleted to save memory. The cleaner that does the deleting is in [05-memory-backend.md](05-memory-backend.md).

## Why the arithmetic copies the Lua script

The Redis backend (Phase 2) runs this same algorithm in a Lua script. The Go code writes the formula in the same order as the script: `cur + prev * (1 - elapsed / window)`.

That is deliberate. Floating-point arithmetic can give very slightly different answers when the same formula is rearranged. Keeping the two in the same order means both backends round the same way and agree on borderline requests, which keeps the Phase 4 comparison fair.

## Check yourself

- Draw the boundary burst and explain why the fixed window allows it.
- Previous window: 60 requests. Current window: 20 so far. You are 30% through. What is the estimate? (Answer: `20 + 60 × 0.7 = 62`.)
- Give a traffic pattern where the estimate is too low, and one where it is too high.
- Why does a rejected request not change the stored state, and how does the code make sure of it?
- Why is Reason B's wait only a lower bound?
- Why does the limiter round `RetryAfter` up and `Remaining` down?
