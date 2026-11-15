# Resume bullets and a 60-second summary

Every number here comes from `bench/results/summary.csv` (scenario in brackets); the table is in [bench/results/RESULTS.md](../bench/results/RESULTS.md). All of it was measured on one laptop under Docker, so quote the comparisons, not the absolute speeds.

Before using any of this, be sure you can explain it. An interviewer will ask "how did you measure that?" and "why 4.5%?". The answers are in `learnings/18-reading-the-results.md` and `learnings/16-the-three-core-pieces.md`.

## Resume bullets

Pick two or three.

- Built a distributed rate-limiting API gateway in Go and compared a Redis + Lua exact limiter with a gossip-based approximate limiter across 3 nodes, measuring 4.5% over-admission against a 33× lower p99 decision latency (12 µs vs 395 µs). [S1]
- Showed the cost of approximation is tunable: gossip over-admission fell from 20% to 0.5% as the gossip interval dropped from 500 ms to 10 ms, within a fifth of a first-order staleness model at intervals of 50 ms and above. [S4]
- Demonstrated the lost-update race the Lua script prevents: a read-then-write limiter admitted over 2,000 requests against a limit of 100; the atomic script admitted exactly 100. [`phase2-naive-race.txt`]
- Built the measurement pipeline: an open-loop load generator, an analyzer that replays each run through a reference limiter, and a one-command runner for 132 runs across 13 scenarios, including injected faults; the pipeline's own error was under 0.04%. [S0]
- Fault-tested both designs: a 5 s Redis outage caused 31% over-admission under fail-open or 28% rejected requests under fail-closed, and a network partition let the gossip cluster admit 33% over its limit. [F2, F3]

The first bullet answers the plan's open question, "which two measured numbers headline the resume bullet": **4.5% over-admission** and **p99 decision latency of 12 µs against 395 µs**.

## 60-second spoken summary

> I built a rate-limiting API gateway in Go, to answer one question: when several gateways share a limit, what does being exact cost?
>
> I built the limiter two ways behind one interface. The exact way keeps the counts in Redis and makes each decision inside a Lua script, so two gateways can never read the same count and both say yes. The approximate way uses no Redis at all: each gateway decides from its own memory and gossips its counts to the others in the background.
>
> Then I measured them with my own load generator, replaying every run through a reference limiter to see how many requests should have got through.
>
> With three gateways at twice the limit, Redis was exact, and each decision took about 400 microseconds at the 99th percentile. Gossip decided in about 12 microseconds, over thirty times faster, but let through 4.5% more than the limit. And that 4.5% is a dial: gossiping every 10 milliseconds instead of every 100 brings it down to half a percent.
>
> So I would use the exact one where the limit is about money or abuse, and the approximate one where latency matters and a few percent over is harmless.

About 190 words, which is a minute at an unhurried pace. Say it aloud a few times; change the words until they are yours.

## Questions to expect, and where the answer is

| Question | Short answer | Read |
| --- | --- | --- |
| Why Lua and not GET then SET? | Two gateways read the same count and both allow. I measured it: over 2,000 admitted against 100 | learnings/08 |
| Why does gossip over-admit? | Each gateway's view of the others is about half a gossip interval old; requests admitted elsewhere in that gap are invisible | learnings/09, 18 |
| How do you merge counts safely? | Nodes send running totals, and a merge keeps the larger. Duplicates, reordering and loss cannot corrupt it | learnings/16 |
| How did you measure over-admission? | Replayed each run's requests through a single exact limiter and compared the counts | learnings/10 |
| How do you know the measurement is right? | A run on one exact node scores under 0.04% error | learnings/13, 18 |
| What happens when Redis is down? | A policy choice: fail-open over-admitted 31%, fail-closed rejected 28% | docs/notes/phase-4 |
| What about a network partition? | Each side enforces the full limit alone: 33% over in a 2 + 2 split | docs/notes/phase-4 |
| What are the limits of your results? | One laptop; relative numbers only; a coarse latency histogram | docs/notes/phase-4 |
