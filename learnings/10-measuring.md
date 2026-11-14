# 10. Measuring: the load schedule and the analyzer

Files: [internal/loadgen/schedule.go](../internal/loadgen/schedule.go), [internal/analyze/analyze.go](../internal/analyze/analyze.go), [cmd/analyze/main.go](../cmd/analyze/main.go)

The project's result is a set of numbers: "gossip over-admits by X% and saves Y ms". Those numbers are only worth something if the measuring is right. A bug in the limiter gives a wrong limiter. A bug in the measuring gives a wrong *conclusion*, and nothing crashes to warn you. That is why this code sits with the correctness-critical work.

Two pieces exist so far. The HTTP side of the load generator and the bench runner are still to come.

## Part 1: when should each request leave?

### Closed loop and open loop

There are two ways to generate load.

**Closed loop:** send a request, wait for the answer, send the next. Simple, and wrong for this job. When the server slows down, the load generator slows down with it. It sends fewer requests just when the server is struggling, so the overload you wanted to measure quietly disappears. This trap has a name: **coordinated omission**.

**Open loop:** decide in advance when every request leaves, "request 0 at 0 ms, request 1 at 5 ms, request 2 at 10 ms", and send each at its time whether or not earlier ones have been answered. Real users behave like this: they do not wait for other users' requests to finish.

The plan requires open loop: "the load generator sends on a fixed schedule and never waits for responses".

### The schedule

```go
func (s Schedule) At(seq int64) time.Time {
	return s.Start.Add(time.Duration(float64(seq) / s.Rate * float64(time.Second)))
}
```

Request number `seq` leaves at `Start + seq / Rate`.

**Why compute it from `seq` every time?** The tempting version keeps a running time and adds one interval per request:

```go
next = next.Add(interval)   // do not do this
```

At 3 requests per second the interval is a third of a second, which cannot be stored exactly. Each addition carries a tiny rounding error, and after a million requests the errors have piled up. Computing from `seq` afresh has one rounding error per request, and they never accumulate. The test `TestScheduleDoesNotDrift` checks that request 3,000,000 lands exactly one million seconds after the start.

### The loop

```go
for seq := int64(0); seq < total; seq++ {
	at := sched.At(seq)
	if wait := at.Sub(timer.Now()); wait > 0 {
		timer.Sleep(wait)
	}
	if timer.Now().Sub(at) > LateAfter {
		late.Add(1)
	}
	sent++
	wg.Go(func() { send(seq, at) })
}
wg.Wait()
```

Three things to notice.

**Each send runs in its own goroutine** (`wg.Go`). The loop starts it and moves straight on. That is what makes the load open loop: a request that hangs for five seconds holds up nothing.

**Falling behind is handled, not hidden.** Suppose the machine freezes for 20 ms. When the loop wakes, several requests are overdue. For each of them `wait` is negative, so there is no sleep and they go out at once, until the loop is back on schedule. No request is skipped and the total stays right.

**Late sends are counted.** A request that leaves more than 5 ms after its scheduled time is "late". A few are normal. Many mean the laptop cannot produce the requested load, so the run is not measuring what it claims. The plan's rule: above 1%, lower the rate and say so in the report.

**The send time that gets recorded is the scheduled time `at`, not the actual one.** That is deliberate. If the generator stalled, the request *should* have left at `at`, and a real user would have been waiting since then.

### Testing code that sleeps

The same trick as the limiter's clock: `Run` does not call `time.Sleep` directly. It takes a `Timer`:

```go
type Timer interface {
	Now() time.Time
	Sleep(d time.Duration)
}
```

The real one sleeps. The test's fake one just moves a number forward, so a "two second" run finishes instantly. The fake can also be told "make the 100th sleep overrun by 20 ms", which lets `TestRunCatchesUpAfterAStall` create a stall on demand and check the exact outcome: 15 requests late, none skipped, schedule recovered.

`TestRunDoesNotWaitForSlowSends` proves the open-loop property directly: every send blocks for ever, and the test checks all 100 still start.

## Part 2: was the limiter right?

### What "right" means

The load generator records every request to a CSV file:

```text
seq, key, send_unix_ns, latency_ns, status, gateway_id
```

From `status` we know what the gateways *did*: 200 means admitted, 429 means rejected. But how many *should* have been admitted?

The answer is to ask a limiter that is known to be exact. Take the single-node memory backend, feed it the same requests in the same order at the same times, and count what it admits. That count is the **ideal**. This is called **replay**.

```go
clock := &replayClock{}
ref := memory.New(clock, memory.WithJanitorInterval(time.Hour))

for _, r := range reqs {
	clock.now = r.Send
	d, err := ref.Allow(context.Background(), r.Key, cfg.Limit, cost)
```

Here the injected clock from [03-limiter-contract.md](03-limiter-contract.md) pays off a second time. Thirty seconds of traffic replay in milliseconds, because the clock is just a variable set to each request's send time. If the limiter read the real clock, this whole method would be impossible.

### The numbers

| Number | Formula | Meaning |
| --- | --- | --- |
| Over-admission % | `max(0, admitted − ideal) / ideal × 100` | Requests let through that an exact limiter would have stopped |
| False rejects % | `max(0, ideal − admitted) / ideal × 100` | Requests stopped that an exact limiter would have let through |
| Worst key over % | The highest over-admission of any single key | One badly over-served client cannot hide among many good ones |
| Peak window ratio | Most admissions for one key in any one-period span, divided by the limit | Catches short bursts that the total smooths away |
| p50, p95, p99 | Latency percentiles | What a typical, a slow and a very slow request experienced |

**Why the worst key as well as the overall figure?** With ten keys, if one is 100% over and nine are perfect, the overall figure is 10%. That hides a client getting double its limit. `TestWorstKeyIsReported` builds exactly that case.

**Why a peak as well as a total?** A limiter could admit 200 in one second and 0 in the next. The total over two seconds is perfect. The peak ratio shows the 2x burst.

### Finding the peak: a sliding span

```go
func peakInSpan(times []time.Time, span time.Duration) int {
	peak, lo := 0, 0
	for hi, t := range times {
		for t.Sub(times[lo]) >= span {
			lo++
		}
		peak = max(peak, hi-lo+1)
	}
	return peak
}
```

`times` is sorted. `hi` walks forward one admission at a time. `lo` trails behind and moves up whenever the gap from `lo` to `hi` reaches one full span. Between them is always the set of admissions inside one span ending at `hi`, and the largest that set ever gets is the peak.

Each index only ever moves forward, so the whole thing is one pass over the list, however long. This **two-pointer** (or sliding window) pattern is a common interview technique in its own right.

### Percentiles

The p99 latency is the value that 99% of requests were at or below. To find it: sort the latencies and pick the one 99% of the way along.

```go
rank := int(math.Ceil(p / 100 * float64(len(sorted))))
return sorted[max(rank, 1)-1]
```

Why percentiles and not the average? Averages hide the bad cases. If 99 requests take 1 ms and one takes 1,000 ms, the average is about 11 ms, which describes none of them. The p99 says plainly that the slowest 1% wait a second.

### Warm-up, and a decision I changed

The first 5 seconds of every run are thrown away: connections are being opened and caches filled, and those numbers are not representative.

The plan's wording was: keep only the measured window, then replay it. I did it slightly differently, and updated the plan to match:

> **Replay the whole run, warm-up included, and count only what falls in the measured window.**

The reason: at the 5-second mark the real gateways are not empty. They hold counts from the previous window. A reference limiter started cold at that moment has an empty previous window, so it admits more in its first second than the real one could. The analyzer would then report false rejects that never happened.

`TestExactRunScoresZero` checks this. It produces a run with a perfect limiter and requires the analyzer to report exactly 0% on both measures. It is the analyzer's own noise-floor test: if a perfect limiter does not score zero, the measuring tool is adding error of its own.

## A bug the tests caught

My first version had one helper for both percentages:

```go
func pctOver(a, b int) float64 { ... return float64(a-b) / float64(b) * 100 }

res.OverAdmissionPct = pctOver(res.Admitted, res.Ideal)   // (admitted − ideal) / ideal: right
res.FalseRejectPct   = pctOver(res.Ideal, res.Admitted)   // (ideal − admitted) / admitted: WRONG
```

Swapping the arguments for the second line looked neat. But it also swapped the *denominator*: false rejects were being divided by the admitted count, where the plan's formula divides by the ideal count.

`TestRejectEverything` caught it. With everything rejected, admitted is 0, the helper refused to divide by zero and returned 0%, where the right answer is 100%.

Look at what that bug would have done in the real project. No crash. A column of plausible numbers in the report, all slightly wrong, and nobody the wiser. The fix was to make the helper take the difference and the ideal count as two separate arguments, so the denominator can no longer be swapped by accident:

```go
res.OverAdmissionPct = pctOfIdeal(res.Admitted-res.Ideal, res.Ideal)
res.FalseRejectPct   = pctOfIdeal(res.Ideal-res.Admitted, res.Ideal)
```

The lesson: test the extremes (everything admitted, everything rejected, nothing at all). Formulas that are wrong in the middle are hard to spot. Formulas that are wrong at the extremes give absurd answers that a test can pin down.

## Running it

```
go run ./cmd/analyze -in bench/results/raw/S1-gossip-1.csv -scenario S1 -backend gossip -rep 1 -gateways 3
```

It prints the figures and appends one row to `bench/results/summary.csv`. It also prints a warning when errors or late sends pass 1% of requests, the plan's rule for "this laptop could not sustain the run".

I tried it on a small made-up CSV: a pretend gateway admitting 110 per second against a limit of 100. It reported 11.1% over-admission and a peak of 1.10x the limit, which is what that input should give.

## Check yourself

- What is coordinated omission, and how does an open-loop generator avoid it?
- Why is `At(seq)` computed from `seq` and not by adding an interval each time?
- What does the loop do after a 20 ms stall, and why does it record the scheduled time?
- How is the "ideal" count obtained, and what makes that possible?
- Give an example where the overall over-admission looks fine but the worst-key figure does not.
- Why replay the warm-up through the reference limiter and then ignore its results?
- What was wrong with `pctOver(res.Ideal, res.Admitted)`, and which test exposed it?
- Why report p99 and not the average?
