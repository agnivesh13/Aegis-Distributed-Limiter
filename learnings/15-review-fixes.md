# 15. Fixing the review findings

Files: [cmd/bench/](../cmd/bench/), [cmd/loadgen/main.go](../cmd/loadgen/main.go), [internal/proxy/proxy.go](../internal/proxy/proxy.go), [internal/config/config.go](../internal/config/config.go), [deploy/docker-compose.yml](../deploy/docker-compose.yml)

[14-code-review.md](14-code-review.md) explains what the review found. This note explains how each finding was fixed, and the one extra problem the fixes uncovered. Read 14 first.

## How every fix was done

The same four steps each time:

1. **Write a test that fails because of the bug.**
2. **Fix the code** until the test passes.
3. **Undo the fix on purpose** and check the test fails again. A test that passes with or without the fix proves nothing. (Note 06 called this mutation testing.)
4. **Check it in the real cluster** where that was possible.

For the proxy fixes, step 3 looked like this: removing each fix made its test fail (1, 1 and 4 failing cases), and putting it back made them pass.

## R1 and the gap it uncovered: refusing a broken run

The bench runner now checks every run before writing its row:

```go
func validRun(backend string, gateways int, f Fault, w Snapshot) error {
	if w.LimiterErrors > 0 && f.StopRedisAt == 0 {
		return fmt.Errorf("the limiter failed %.0f times in a run with no outage ...")
	}
	if backend == "gossip" && gateways > 1 && w.GossipMerged == 0 {
		return errors.New("the gossip gateways merged nothing from each other ...")
	}
	return nil
}
```

**The first rule** is the review's R1: limiter errors are only acceptable when the run deliberately stops Redis.

**The second rule was found while fixing R1.** A gossip run with your `Merge` still a stub has *no* limiter errors: every gateway decides happily from its own counts. So R1's check alone would still let it write a row showing large over-admission, labelled "gossip", as if that were the design's result. It is the same family of bug as R1, and the review had missed it.

To catch it, each gossip node now counts how many received counts told it something new (`gossip_entries_merged_total`). Three gateways that merged nothing in a whole run are not running the design being measured, so the run is refused. One gateway alone is fine: it has nobody to merge from.

**Proof in the real cluster.** Running S1 once per backend now gives:

```text
[1/2] S1-default-redis-rep1
  FAILED: the limiter failed 59988 times in a run with no outage ...
[2/2] S1-default-gossip-rep1
  FAILED: the gossip gateways merged nothing from each other, so each limited alone; is Store.Merge written?
summary rows before: 4, after: 4
```

Before this, both runs would have added a row each. Both reasons are your unwritten pieces, and the message says so.

**Lesson:** after fixing a bug, ask "what else has the same shape?" Fixing exactly the reported case and stopping there is how the second case survives.

## R2: subtract per gateway

`scrape` now returns one snapshot per gateway (a `Scrape`, a map from admin address to `Snapshot`), and `Window` subtracts gateway by gateway:

```go
for addr, a := range after {
	b, ok := before[addr]
	if !ok {
		continue        // missing from either scrape: leave it out
	}
	out.plus(a.Sub(b))
}
```

The test is the review's reproduction turned into a check: three gateways before, two after. The result is now 800 observations from the two survivors, not −200.

## R3: no setting can be silently ignored

Two halves:

- **Compose now lists all eleven variables** the gateway reads. A test reads the Compose file and fails if any name from `config.EnvNames()` is missing, so the list cannot drift again.
- **The scenario loader rejects an `env` key the gateway does not read.** A typo such as `GOSSIP_INTERVL` used to run with the default interval. Now it stops the run before anything starts.

Notice the single source of truth: both checks use `config.EnvNames()`, the same list the config loader uses. Nobody has to keep three lists in step by hand.

## R4: the memory backend runs alone

Scenarios can now give one backend its own gateway count:

```yaml
  - id: S9
    backends: [memory, redis, gossip]
    backend_gateways: { memory: 1 }
```

The loader also refuses any scenario that would put the memory backend on more than one gateway, so the mistake cannot be made again elsewhere.

## R5: one instant, shared

The load generator has a new flag, `-start-at`, which takes an exact moment (in nanoseconds since 1970). The runner picks "now plus 5 seconds", passes it to the load generator, and times faults and the warm-up snapshot from the same moment:

```go
start := time.Now().Add(loadLead)
faults := scheduleFaults(ctx, c, env, admins, r.Fault, start)
... j.LoadgenArgs("/results/raw/"+csvName, start) ...
```

Why does this work across a container boundary? Containers read the host machine's clock, so "this instant" means the same thing inside and out. The real run confirmed it: zero late sends, and the measured window held 59,988 requests where 60,000 were expected after the warm-up.

**A simplification fell out of it.** The burst shape used to sleep through its silent part. With a fixed start time there is no need: the silent segment just pushes the next segment's start later, and the scheduler already sleeps until each send's time. One fewer moving part.

## R6: the last X-Forwarded-For entry

```go
last := xff[strings.LastIndexByte(xff, ',')+1:]
```

`LastIndexByte` returns −1 when there is no comma, and `-1 + 1 = 0`, so a single-entry header gives the whole string. One line covers both cases. A new test sends three requests with a different made-up first entry each time and checks all three are charged to the real address.

## R7, R8, R9, R10: small and direct

- **R7:** an API key longer than 128 bytes gets `400` before the limiter is asked.
- **R8:** the metric's help text now says what it measures: "Time to proxy a request to the upstream and relay its whole response to the client."
- **R9:** if the request's own context is done when the limiter call fails, the client has gone, so nothing is counted and nothing is written. The test uses a limiter that blocks like a slow Redis, cancels the request mid-call, and checks the error counter never appears.
- **R10:** `analyze.Percentile` is exported and the load generator calls it. One function, one answer.

## R11: telling "left out" from "zero"

```go
Cost *int `yaml:"cost"`

func (r Route) CostValue() int {
	if r.Cost == nil {
		return 1
	}
	return *r.Cost
}
```

The same pointer trick as `warmup` in the scenario file ([13-bench-runner.md](13-bench-runner.md)): `nil` means "not written", a pointer to 0 means "written as 0", which validation now rejects.

## R12 and R13

- **R12:** before its first run, the bench runner removes any leftovers of its own and then tries to connect to each host port the gateways use. If something answers, it stops with one clear line. Tested for real by starting a dev cluster first.
- **R13:** the README says the Grafana login is for local use only.

## Check yourself

- Why undo a fix on purpose after its test passes?
- R1 checked limiter errors. Why was that not enough for gossip, and what signal catches the gossip case?
- How does `Window` stay correct when a gateway dies, and why did summing first fail?
- What single list do the Compose check, the scenario check and the config loader share, and why does that matter?
- Why can the runner and a container agree on "start at this instant"?
- Trace `xff[strings.LastIndexByte(xff, ',')+1:]` for `"8.8.8.8"` and for `"9.9.9.9, 8.8.8.8"`.
- What is the difference between a `Cost` of `nil` and a `Cost` pointing at 0?
