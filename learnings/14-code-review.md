# 14. Reviewing code: what Fable found in the gateway and bench runner

File: [docs/reviews/2026-11-15-gateway-cluster-bench.md](../docs/reviews/2026-11-15-gateway-cluster-bench.md)

After Opus built the gateway, the cluster and the bench runner, Fable reviewed that work. This note explains how the review was done and what each finding means, because reviewing code is a skill of its own and interviewers ask about it.

## What a review is for

All of Opus's tests passed. So why review?

Tests answer "does the code do what its author thought of?" A review asks "what did the author *not* think of?" The two catch different bugs. Every finding below is in code whose tests are green.

## How the review was done

1. **Read with questions, not line by line.** For each piece: What happens when an input is hostile? When a dependency disappears half-way? When this number is zero, or missing? When two things happen in the other order?
2. **Follow the data to where it ends up.** The bench runner's output is a row in `summary.csv` that will be quoted in a report. So the question for every step was: "can this put a wrong number in that row without anyone noticing?"
3. **Check a suspicion before writing it down.** Each finding was confirmed against the code, and the worst one was reproduced with a throwaway test. A review full of "this might be wrong" wastes the reader's time.
4. **Say what to do and how to test it.** A finding that only says "this is bad" leaves the work to someone else.
5. **Say what is good, too.** Otherwise the next change may "fix" something that was right.

## The findings, explained

The review numbers them R1 to R13. The ones that teach something:

### Silent wrong answers (R1, R3, R4)

Three findings have the same shape: **the program finishes happily and writes a number that is wrong.**

- **R1:** if the backend cannot decide (today, because a script is a stub), `fail_open` lets everything through and the runner records "100% over-admission" as if it were a measurement.
- **R3:** the Compose file passes only six of the eleven environment variables to the gateway. A scenario that sets one of the other five runs with the default and is labelled with the value it never used.
- **R4:** three memory-backend gateways do not share a limit, so that scenario's accuracy figure is meaningless, and it would sit in the same table as real ones.

Crashes are easy: you see them. These are the dangerous kind, and in measuring code they are the main thing to hunt for. The fix has the same shape each time: **make the program refuse, or shout, instead of carrying on.**

### Subtracting things that are not the same things (R2)

The runner scrapes every gateway's counters twice and subtracts, to get what happened in between. It added the gateways together first, then subtracted.

That works until a gateway is killed, which is exactly what scenario F1 does. "Before" has three gateways, "after" has two, and the difference is negative. I reproduced it: bucket counts of −100 and −200, and a p99 of 0.

The rule: **subtract like from like.** Take each gateway's difference separately, drop any gateway missing from either side, then add up.

### Measuring from the wrong moment (R5)

The runner noted the time, *then* started the load generator's container, which takes a second or two to start. Faults were timed from the note, so "kill a gateway at 10 s" really happened 8 or 9 seconds into the load.

**Lesson:** when two programs must agree on a moment, pass the moment between them explicitly (here: tell the load generator when to start). Do not assume starting something is instant.

### Trusting a header the client wrote (R6)

`X-Forwarded-For` lists the addresses a request passed through. Each proxy **appends** the address it saw:

```text
client sends:          X-Forwarded-For: 1.2.3.4          (made up)
load balancer appends: X-Forwarded-For: 1.2.3.4, 203.0.113.9   (real)
```

The gateway took the **first** entry. That is the part the client controls. A client could send a new made-up address with every request and never be limited.

The entry to trust is the **last** one: it was written by your own load balancer. This is a classic security question about rate limiters, and now you have seen the bug in your own project.

### Input with no size limit (R7)

The API key is used as part of a map key, a Redis key and a gossip packet. Nothing limited its length. A client could send a 100 KB key on every request and fill memory.

**Lesson:** any value that arrives from outside and gets *stored* needs a size limit.

### A metric that does not measure what it says (R8)

The help text said "time until the upstream's response headers". The timer actually stopped after the whole response body had been sent to the client. With a slow client, the "upstream latency" graph would rise although the upstream was fine. A metric that misleads is worse than a missing one, because people act on it.

### Blaming the wrong party (R9)

If a client hangs up while the gateway is asking Redis, the Redis call is cancelled, and the gateway counted that as a limiter error. Under load, with impatient clients, the dashboard would show Redis failing when Redis was healthy. The fix is to check *why* the call failed before counting it.

### Two implementations of one thing (R10)

The load generator and the analyzer each had their own percentile function, rounding slightly differently. They could print different p99s for the same run. The same lesson as moving the sliding window arithmetic into one shared file: **one job, one piece of code.**

## Why the fixes were left for Opus

The review found the problems and wrote down each fix and test, but did not apply them. That is deliberate: in this project the split is "Opus builds this code, Fable reviews it", the same way a reviewer on a team comments on a pull request and the author makes the changes. The author learns what was missed, and the reviewer's time goes to the next review. The follow-ups are listed in [TASKS.md](../TASKS.md) as R1 to R13.

## Fable's own tasks

- **The S0 noise floor was checked and passes:** at most 0.034% error against a limit of 1%. That means the measuring pipeline adds almost no error of its own, and the later results can be trusted to that precision.
- **The 1.05× peak** seen in S0 belongs to the sliding window algorithm itself (it appears on one exact node), so it is a floor shared by every backend and should be stated in the report.

## Check yourself

- All the tests passed. Name two findings and say why no test caught them.
- Why are "silent wrong answers" worse than crashes in measuring code?
- Explain R2 with three gateways before and two after. What is the correct way to subtract?
- A request arrives with `X-Forwarded-For: 5.5.5.5, 10.0.0.7`. Which address should a gateway behind one trusted load balancer limit, and why?
- Why does a stored input need a size limit when a merely read input may not?
- Give an example of a metric that would mislead an operator, and what they might wrongly do.
