# 19. The write-up, and a better model

Files: [README.md](../README.md), [docs/report.md](../docs/report.md), [docs/notes/phase-5.md](../docs/notes/phase-5.md)

Phase 5 turns the numbers into something a stranger can read. This note covers how the two documents are organised, the one new idea that came out of writing them (a correction to the prediction formula), and how the numbers were checked.

## Two documents, two readers

| Document | Reader | What they want |
| --- | --- | --- |
| README | Someone who landed on the repo and will give it two minutes | What is this, what did it find, can I run it |
| Report | Someone deciding whether to believe the result | How it was measured, every scenario, where it could be wrong |

So the README opens with the result, not with installation steps. A recruiter who reads only the first screen still leaves with "exact costs 395 µs, approximate costs 4% accuracy". The report can afford to be long because its reader chose to open it.

Both state their limits plainly. A results page with no limitations section reads as if the author did not know them.

## The correction to the model

Note 18 compared the plan's formula with the measurements and found the formula always a little low. Writing the report meant asking *why*, and one cause turned out to be a flaw in the formula itself.

### The original reasoning

A gateway cannot see what the other gateways admitted in the last `lag` seconds. The plan counted those hidden requests like this:

```text
hidden = (share of traffic at other gateways) × (limit per second) × lag
p      = ((N − 1) / N) × lag / W
```

### The flaw

"Limit per second" is wrong. The other gateways are not admitting at the limit. They are admitting at the limit *plus the over-admission*, because they are over-admitting too. The hidden amount is bigger by exactly the thing we are trying to compute.

Call the over-admission `x`. The real admit rate is `(1 + x)` times the limit, so:

```text
x = p × (1 + x)
```

This is an equation with `x` on both sides. Solve it:

```text
x − p·x = p
x (1 − p) = p
x = p / (1 − p)
```

### Does it matter?

When `p` is small, dividing by `(1 − p)` changes almost nothing: 3.33% becomes 3.45%. When `p` is large it matters a lot:

| Interval (3 gateways) | Old formula | Corrected | Measured |
| --- | --- | --- | --- |
| 100 ms | 3.33% | 3.45% | 3.86% |
| 500 ms | 16.7% | 20.0% | 20.1% |

At 500 ms the old formula missed by a fifth. The corrected one is almost exact.

This pattern has a name: a **feedback loop**. The error feeds itself. More over-admission means more hidden requests, which means more over-admission. Such loops settle at `p / (1 − p)`, the same shape as the sum `p + p² + p³ + …`.

### What is left over

Turn each measurement back into a lag and compare it with half the interval:

| Interval | Half the interval | Lag the measurement implies | Extra |
| --- | --- | --- | --- |
| 10 ms | 5 ms | 8 ms | 3 ms |
| 50 ms | 25 ms | 29 ms | 4 ms |
| 100 ms | 50 ms | 56 ms | 6 ms |
| 500 ms | 250 ms | 251 ms | 1 ms |

A few milliseconds each time. That is what delivery and processing would cost. But be careful how strongly you say it: the three runs at 100 ms alone imply lags from 48 to 62 ms. The extra is about the same size as the noise, so "consistent with a few milliseconds of delivery time" is honest and "delivery takes 4 ms" is not.

### Fanout as a lag

The same trick explains five gateways. At 100 ms:

| Fanout | Implied lag |
| --- | --- |
| 4 (reaches everyone) | 59 ms |
| 2 | 80 ms |
| 1 | 142 ms |

With fanout 4 the lag is 59 ms, the same as with three gateways (56 ms). So the model works whenever every round reaches every peer, and a smaller fanout shows up as a longer lag.

## Things the model cannot explain

The report says so, and you should too:

- Over-admission was higher at 6× load (5.08%) than at 2× (3.83%). The model says load should not matter. The laptop was near its ceiling at 6×, so gossip may have been late, but nobody tested that.
- At five gateways the lag is a full interval at 10 ms but 0.61 of one at 500 ms. No tested explanation.

Saying "I do not know why, and here is what I would run to find out" is a strong answer in an interview. Inventing a reason is a weak one.

## How noisy is one gossip number?

The default setup (3 gateways, 100 ms, fanout 2, twice the limit) happens to be a point in four scenarios, so it was measured 12 times. Those runs range from 3.13% to 4.61%. The headline 4.45% came from the highest of the four medians; the others were 3.74%, 3.83% and 3.86%.

So "about 4%" is the fair way to say it aloud, and a half-point difference between two gossip results means nothing.

## Checking the numbers

Plan rule 7: every number in the README and report must come from a file in `bench/results/`. The check was a separate step after writing:

1. A script computed every median, range and derived figure from `summary.csv`.
2. Each number in the two documents was compared against that output.
3. The report ends with a table saying which file each kind of number comes from.

This caught four mistakes in the first draft (problem 33 in [07-problems-and-fixes.md](07-problems-and-fixes.md)).

The README's reproduce command was also run for real, not just read: it produced a row with 0.00% over-admission and a 1.05 peak, matching the committed S0 row.

## Check yourself

- Why does the README start with results and not with how to install?
- In one sentence, what was wrong with the plan's formula?
- Solve `x = p (1 + x)` for `x`.
- For which gossip intervals does the correction matter, and why only those?
- The extra lag came out at 1 to 6 ms. Why should you not quote that as a measured delivery time?
- What does a fanout of 1 do to the lag, in numbers?
- Name one result the model does not explain, and what you would run next.
- The same setup gave medians of 3.74% and 4.45%. What does that tell you about comparing two gossip numbers?
