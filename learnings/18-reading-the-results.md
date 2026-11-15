# 18. Reading the results

Files: [bench/results/RESULTS.md](../bench/results/RESULTS.md), [docs/notes/phase-4.md](../docs/notes/phase-4.md), [docs/resume.md](../docs/resume.md), the two charts in [bench/results/](../bench/results/)

The whole project exists to produce these numbers. This note explains what each one means, why it came out as it did, and what went wrong on the way to getting them. If you can explain this note, you can defend the project.

## The one-line answer

**With 3 gateways at twice the limit, the exact limiter (Redis) took about 395 µs per decision and never let an extra request through. The approximate limiter (gossip) took about 12 µs and let 4.5% extra through.**

That is the trade the project set out to measure: exactness costs time on every request.

## How to read one row

```text
S1  default  gossip  3 gateways  3 runs | over 4.45 (3.71 to 4.61) | decision p99 12 (11 to 12)
```

- **S1** is the scenario; **default** is the point within it.
- **3 runs:** each point is run three times, because one run on a laptop is noisy.
- **4.45 (3.71 to 4.61):** the **median** of the three, then the lowest and highest. The median is the middle value; one odd run cannot drag it the way it would drag an average.
- **over** is over-admission: how many more requests got through than a perfect limiter would allow, as a percentage.
- **decision p99:** 99% of limiter decisions took this long or less, in microseconds.

A microsecond (µs) is a millionth of a second. A millisecond (ms) is a thousandth. So 395 µs is 0.395 ms.

## Why Redis is exact and slower

Every decision is a round trip to Redis: send the script call, wait, read the reply. That takes time, and how much depends on the network. S6 shows it directly. A tool (toxiproxy) added a fixed delay to every Redis reply:

| Delay added | p99 decision latency |
| --- | --- |
| 0 ms | 0.74 ms |
| 1 ms | 2.9 ms |
| 5 ms | 6.6 ms |

The limiter gets slower as Redis gets further away. In a real deployment, where Redis sits on another machine, this is the number that grows.

In exchange, Redis is exact everywhere: 0.00% over-admission with 1, 3 and 5 gateways (S2). The Lua script makes each decision a single unbreakable step, so it does not matter how many gateways ask.

## Why gossip is fast and over-admits

A gossip gateway decides from its own memory. No network call, so 10 to 12 µs, the same as the plain memory backend.

But its picture of the other gateways is always a little old. Suppose gateways gossip every 100 ms. On average, what a gateway knows about the others is about 50 ms out of date. Requests the others admitted in those 50 ms are invisible to it, so it admits some that a perfect limiter would refuse.

### The prediction, and how close it came

The plan gave a rough formula:

```text
over-admission ≈ ((N − 1) / N) × lag / W
```

- `N` is the number of gateways. `(N − 1) / N` is the share of traffic that goes to *other* gateways, the part a gateway cannot see straight away.
- `lag` is how stale its view is: about half the gossip interval.
- `W` is the window, 1 second.

For 3 gateways gossiping every 100 ms: `(2/3) × 0.05 / 1 = 3.3%`. Measured: **3.86%**.

| Interval (3 gateways) | Predicted | Measured |
| --- | --- | --- |
| 10 ms | 0.33% | 0.54% |
| 50 ms | 1.67% | 1.99% |
| 100 ms | 3.33% | 3.86% |
| 500 ms | 16.7% | 20.1% |

The measurement is always a little above the prediction. That makes sense: the formula counts only the wait until the next gossip round, and leaves out the time a packet takes to arrive and be processed. At 10 ms that leftover is a large share of the total, so the relative gap is biggest there.

This is the first chart: over-admission against gossip interval. It is the most important picture in the project, because it shows that **the cost of approximation is a dial**. Gossip ten times as often and the error falls about seven-fold, at the price of more network traffic.

### Where the simple formula breaks: fanout

With 5 gateways at 100 ms, the formula says 4.0%. Measured: **6.3%**. Why so far off?

The formula assumes each gossip round reaches *every* other gateway. With the default fanout of 2, a round reaches only 2 of the 4 others. The rest hear the news second-hand, a round or two later, so their view is staler than the formula thinks.

Scenario S5 tests exactly this, at 5 gateways and 100 ms:

| Fanout | Peers reached each round | Over-admission |
| --- | --- | --- |
| 4 | all 4 | 4.97% |
| 2 | 2 of 4 | 6.86% |
| 1 | 1 of 4 | 12.8% |

With fanout 4 the formula's assumption holds, and the measurement (4.97%) is close to its 4.0%. This is a good thing to say in an interview: "my model was off at 5 nodes, I worked out why, and I had a scenario that confirmed the reason."

## The faults

**Redis outage (F2).** Redis was stopped for 5 seconds in the middle of a run. What happens is a choice you make in the config:

| Policy | During the outage | Result |
| --- | --- | --- |
| `fail_open` | Let everything through | 30.6% over-admission, no client saw an error |
| `fail_closed` | Refuse everything | 0% over-admission, 27.7% of requests got a 503 |

Neither is "right". Fail open if the service must stay up (a product page). Fail closed if the limit protects something (a payment API).

**Partition (F3).** Four gossip gateways were split two against two for 10 seconds, so the halves could not hear each other. Each half then enforced the *full* limit by itself: 33.4% over-admission, with a peak of 2.15 times the limit in one second. That is the price of having no central authority: when the network splits, nobody knows what the other side admitted.

**Gateway crash (F1).** One of three gateways was killed. The survivors kept limiting correctly. The 27% errors are simply requests that were sent to the dead gateway.

## Two things the results say about the algorithm itself

- **The 1.05 peak.** Even one exact gateway (S0) let 1.05 times the limit through in some one-second span. The sliding window counter *assumes* the previous window's requests were evenly spread ([04-sliding-window.md](04-sliding-window.md)); when they were not, it is a little off. Every backend shares this, so compare peaks against 1.05, not against 1.00.
- **The noise floor.** S0's over-admission and false rejects are under 0.04%. So the measuring pipeline adds almost nothing of its own, and a difference of a few percent between backends is real.

## What these numbers cannot tell you

Say these yourself before an interviewer has to ask:

- **One laptop.** The gateways and the load generator share the same CPU. The *comparisons* hold; the absolute speeds would differ on real servers. "Thirty times faster" is safer to quote than "12 microseconds".
- **A coarse ruler.** Decision latency comes from a histogram whose buckets grow by 1.5× each. A p99 is known only to within its bucket. Memory and gossip both sit in the first bucket, so "about 10 µs" really means "under 10 µs".
- **The limiter is not the bottleneck.** In S9 everything collapsed between 8,000 and 16,000 requests per second, for all three backends, because the laptop ran out of CPU. That is the machine's ceiling, not the limiters'.

## Problems met while getting the numbers

Logged in [07-problems-and-fixes.md](07-problems-and-fixes.md) as numbers 29 to 32. In short:

- **Every five-gateway run failed to start.** The upstream service's host port (8081) was inside the range reserved for gateways (8080 to 8084), so only four gateways could ever get a port. Nothing had run five gateways before, so neither testing nor the code review caught it.
- **A safety rule was too strict.** The runner refused any run with a limiter error. But Redis missing its 50 ms deadline a handful of times under heavy load is a *result*. The rule now refuses only runs where nearly every request failed.
- **The laptop could not send 10,000 requests per second on time.** The plan's rule is to lower the rate and say so, so the top load step became 6× and the burst 4×.
- **The first draft of the write-up overstated four things,** found by checking each sentence against the table before committing.

## Check yourself

- State the headline result in one sentence, with both numbers.
- Why is a median of three runs reported, and not an average or a single run?
- Using the formula, predict over-admission for 3 gateways at a 50 ms interval. (1.67%.) What did the measurement show, and why is it a little higher?
- Why did the formula miss badly at 5 gateways, and which scenario proves your explanation?
- Redis was exact at 5 gateways. What did it cost, and which scenario shows that cost growing?
- When would you choose `fail_open`, and when `fail_closed`?
- What does the 33% in the partition scenario mean, in plain words?
- Name two limits of these results.
