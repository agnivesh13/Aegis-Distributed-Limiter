# 16. The three core pieces: token bucket, Lua script, merge

Files: [token_bucket.go](../internal/limiter/memory/token_bucket.go), [sliding_window.lua](../internal/limiter/redis/scripts/sliding_window.lua), [merge.go](../internal/limiter/gossip/merge.go)

The plan set these three aside for you to write by hand, because they are what an interviewer will ask about. You handed them to Fable instead. That makes this note the important one: **you did not type these, so you need to be able to rebuild each from memory.** For every piece there is a "rebuild it" exercise at the end. Do them. Being able to explain code you did not write is a normal part of the job, but only if you really can.

## Piece 1: the token bucket

### The idea

Each client has a bucket. It holds up to `capacity` tokens. Every request takes `cost` tokens out. Tokens drip back in at a steady `rate`. If the bucket does not hold enough, the request is rejected.

```text
capacity 20, rate 10 tokens per second

t = 0.0 s   bucket: 20   20 requests arrive   → all allowed, bucket: 0
t = 0.0 s   bucket:  0   1 more               → rejected, "retry in 0.1 s"
t = 0.1 s   bucket:  1   1 request            → allowed, bucket: 0
t = 5.0 s   bucket: 20   (full again; it never goes above 20)
```

Two numbers control it, and they mean different things:

- **`rate`** is the long-run average the client may sustain.
- **`capacity`** (the burst) is how much it may send in one instant after being quiet.

That is the difference from the sliding window: the token bucket *allows bursts on purpose*, up to the bucket size, while holding the average.

### Lazy refill: the trick

Nothing actually drips. There is no timer adding tokens. Each time a request arrives, the code asks "how long since I last looked, and how many tokens arrived in that time?"

```go
elapsed := max(0, now.Sub(bk.ts).Seconds())
bk.tokens = math.Min(capacity, bk.tokens+elapsed*rate)
```

So the state is only two numbers per client: `tokens` and `ts` (when `tokens` was last correct). A million idle clients cost no CPU at all. With a timer per client they would.

### The code, step by step

```go
capacity := float64(l.Capacity())
rate := float64(l.Rate) / l.Period.Seconds() // tokens per second
```

`Rate: 50, Period: 1s` gives 50 tokens per second. `Rate: 100, Period: 1m` gives 1.67 per second.

**Step 1: a new client starts full.**

```go
bk := s.buckets[key]
if bk == nil {
	bk = &bucket{tokens: capacity, ts: now}
	s.buckets[key] = bk
}
```

**Step 2: refill**, shown above. Two guards live in that one pair of lines:

- `math.Min(capacity, ...)`: an hour of idleness must not leave 36,000 tokens.
- `max(0, ...)`: if the machine's clock steps backwards, elapsed time would be negative and tokens would be *removed*. Treat it as no time passing.

**Step 3: decide.**

```go
if bk.tokens >= c {
	d.Allowed = true
	bk.tokens -= c
} else {
	d.RetryAfter = max(1, ceilSeconds((c-bk.tokens)/rate))
}
```

On a reject: the client is short by `c − tokens` tokens, which arrive at `rate` per second, so the wait is `(c − tokens) / rate` seconds. With 0 tokens, a cost of 5 and a rate of 10: half a second.

**Step 4: store the time, allowed or not.**

```go
bk.ts = now
```

The refilled count was already written in step 2. Updating `ts` even on a reject is what keeps the next refill from counting the same stretch of time twice.

**Step 5: what the client is told.**

```go
d.Remaining = int(math.Floor(bk.tokens))
d.ResetAfter = ceilSeconds((capacity - bk.tokens) / rate)
```

2.7 tokens means 2 whole requests, so round *down*. `ResetAfter` is the time until the bucket is full again.

**Step 6: when can the key be forgotten?**

```go
bk.expires = now.Add(ceilSeconds(capacity / rate))
```

After `capacity / rate` idle seconds the bucket would be full. A missing key also means "full" (step 1). The two are the same, so the janitor may delete the key and nothing changes.

### Rounding, again

`ceilSeconds` rounds waits *up* to the next nanosecond. A client that waits exactly `RetryAfter` must be let in; rounding down could leave it a hair short. `Remaining` rounds *down* for the same kind of reason: never promise more than there is.

### Token bucket or sliding window?

| | Token bucket | Sliding window counter |
| --- | --- | --- |
| Bursts | Allowed, up to the bucket size | Smoothed away |
| On one node | Exact | Approximate (assumes even spread) |
| State per key | Tokens and a timestamp | Two counts and a window number |
| Merges between nodes | **No** | **Yes** |

The last row is why the gossip backend refuses the token bucket. Two nodes each holding "7.5 tokens as of 12:00:01" cannot be combined into one truth: tokens go up *and* down, so there is no "larger is newer" rule. Counts in a window only go up, which is what makes Piece 3 possible.

### Rebuild it

Close this note. On paper, write the six steps as six lines of plain English. Then, for capacity 10 and rate 2 per second, work out by hand: 10 requests at t=0, 1 request at t=0.4 s, 1 request at t=0.5 s. (Answers: all 10 allowed; rejected with 0.8 tokens, retry in 0.1 s; allowed, leaving 0.)

## Piece 2: `sliding_window.lua`

This is the sliding window counter from [04-sliding-window.md](04-sliding-window.md), written in Lua so it can run *inside* Redis. Why it must run there is in [08-redis-backend.md](08-redis-backend.md): reading and writing from Go lets two gateways both read 99 and both allow, and that approach admitted over 2,000 against a limit of 100 when tested.

Read the script with note 04 beside it. The steps are the same; only the language differs.

### Inputs

```lua
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local cost   = tonumber(ARGV[3])
```

`KEYS[1]` is the Redis key for this client. Everything is text when it arrives, so `tonumber`. Lua lists start at 1.

### The time comes from Redis

```lua
local now = tonumber(ARGV[4])
if now == nil then
  local t = redis.call('TIME')
  now = t[1] * 1000 + math.floor(t[2] / 1000)
end
```

`TIME` returns seconds and microseconds; this makes milliseconds. Using Redis's clock means three gateways with slightly different clocks cannot disagree about which window it is. The fourth argument exists only so tests can set the time.

### Steps 1 and 2: which window, and bring the counts up to date

```lua
local idx     = math.floor(now / window)
local elapsed = now - idx * window

local state = redis.call('HMGET', KEYS[1], 'idx', 'cur', 'prev')
local sidx  = tonumber(state[1])
local cur   = tonumber(state[2]) or 0
local prev  = tonumber(state[3]) or 0
if sidx == nil or sidx < idx - 1 or sidx > idx then
  cur = 0
  prev = 0
elseif sidx == idx - 1 then
  prev = cur
  cur = 0
end
```

The state is a Redis **hash** with three fields. `tonumber(nil) or 0` handles a client seen for the first time.

The three cases are the table from note 04: same window, keep both; one window old, the old `cur` becomes `prev`; anything else, both are zero. `sidx > idx` covers stored state from the *future*, which can only mean a clock went backwards; it is discarded too.

These are local variables. Nothing has been written yet.

### Step 3: estimate, and decide

```lua
local estimate = cur + prev * (1 - elapsed / window)

if estimate + cost <= limit then
  allowed = 1
  cur = cur + cost
  estimate = estimate + cost
  redis.call('HSET', KEYS[1], 'idx', idx, 'cur', cur, 'prev', prev)
  redis.call('PEXPIRE', KEYS[1], window * 2)
```

Only the allowed branch calls `HSET`. A rejected request writes nothing, which is how "a rejected request consumes nothing" is guaranteed. `PEXPIRE` tells Redis to delete the key two windows after the last write; Redis does the janitor's job itself.

### The two kinds of reject

```lua
elseif cur + cost <= limit then
  retry_ms = math.max(1, math.ceil(window * (1 - (limit - cur - cost) / prev) - elapsed))
else
  retry_ms = window - elapsed
end
```

The same two reasons as in Go: the previous window's weight is in the way (wait for it to decay; the formula is derived in note 04), or the current window alone is full (wait for it to end).

`math.max(1, ...)` is one small thing I added to the plan's printed version. Floating-point rounding could make the computed wait come out as 0 for a request that was just rejected. "Rejected, retry in 0 ms" is nonsense, so a rejection always says at least 1 ms. The Go code does the same.

### Why everything is whole milliseconds

```lua
return {allowed, remaining, retry_ms, reset_ms}
```

Redis cuts the fraction off any number a script returns. Returning 0.5 seconds would arrive as 0. So the script works in milliseconds and rounds up with `math.ceil` before returning.

### How we know it is right

The Redis backend runs the *same* conformance suite as the memory backend, against a real Redis. Every sliding window case passes, and so does the test where three limiter instances fire 10,000 concurrent requests and exactly 100 get through. In the real cluster, three gateways admitted 3,000 where the ideal was 3,000.

### Rebuild it

Write the script's skeleton from memory as comments only: inputs, time, window, rotate, estimate, allow-and-store, two rejects, return. Then answer aloud: why is there no `HSET` on the reject path? Why `TIME` and not a timestamp from the gateway? Why milliseconds?

## Piece 3: the gossip merge

### What an entry says

Gateways send each other **running totals**, never increments:

> "For key `api:key-7`, in window 1794757029, gateway-2 has admitted 23 requests so far."

That is one `Entry`. [09-gossip-backend.md](09-gossip-backend.md) explains why totals: an increment delivered twice is counted twice, a total delivered twice changes nothing.

### The whole function

```go
func (s *Store) Merge(e Entry) (changed bool) {
	if e.WindowMs <= 0 {
		return false
	}

	idxNow, _ := limiter.WindowAt(s.clock.Now(), time.Duration(e.WindowMs)*time.Millisecond)
	if e.Idx < idxNow-1 || e.Idx > idxNow+1 {
		return false
	}

	sh := s.shardFor(e.Key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	ks := sh.keys[e.Key]
	if ks == nil {
		ks = &keyState{windowMs: e.WindowMs, windows: make(map[int64]*windowCounts)}
		sh.keys[e.Key] = ks
	} else if ks.windowMs != e.WindowMs {
		return false
	}

	wc := ks.window(e.Idx)
	old := wc.perNode[e.Node]
	if e.Count <= old {
		return false
	}
	wc.total += e.Count - old
	wc.perNode[e.Node] = e.Count
	wc.dirty[e.Node] = s.retransmit
	return true
}
```

### Line by line

**Which windows are accepted.** Only the previous, the current and the next.

- Older than the previous window: it cannot affect any decision, so storing it wastes memory.
- The next window: a peer's clock may be a little ahead of ours. Accept it.
- Further ahead: a broken clock or a corrupt packet. Without this limit, one bad packet claiming a window a year away would sit in memory.

**The window length must match.** If this node already counts the key in 1-second windows and the entry is in 10-second windows, the window numbers mean different things and cannot be compared. Ignore it.

**The heart: keep the larger.**

```go
old := wc.perNode[e.Node]
if e.Count <= old {
	return false
}
```

A node's count for a window only ever grows. So of two reports about the same node, the larger is the newer. Walk the three network faults through these lines:

| What happened | `e.Count` against `old` | Result |
| --- | --- | --- |
| Duplicate packet | equal | ignored |
| Old packet arriving late | smaller | ignored |
| A packet was lost | the next one is larger | accepted; nothing lost for good |

Note `<=`, not `<`: an equal count is not news.

**Keep the total right.**

```go
wc.total += e.Count - old
wc.perNode[e.Node] = e.Count
```

`total` is the sum of all the per-node counts, kept ready so a decision does not add them up each time. If gateway-2 goes from 17 to 23, the total rises by 6. Forgetting this line was one of the bugs I injected to test the tests: ten tests failed.

**Pass the news on.**

```go
wc.dirty[e.Node] = s.retransmit
```

Marks this count to be sent in the next few gossip rounds. That is how news reaches gateways the original sender did not pick: each hearer tells others. Only *new* information is relayed (the function returned earlier otherwise), so old news does not circulate for ever.

### The three properties

Because the merge is "take the maximum", it is:

- **Commutative:** `max(a, b) = max(b, a)`. Order of arrival does not matter.
- **Associative:** `max(max(a, b), c) = max(a, max(b, c))`. How packets were grouped does not matter.
- **Idempotent:** `max(a, a) = a`. Receiving something twice does not matter.

Data with a merge like this is a **CRDT**; this particular one is a **G-Counter** (grow-only counter). `TestMergeProperties` checks all three on 200 random inputs each.

### It applies to yourself, too

There is no special case for `e.Node == s.self`. If a gateway restarts with empty memory, its peers still hold its old count and send it back. It merges like any other entry, and the gateway carries on counting from where it was. `TestRestartedNodeRelearnsItsOwnCount` shows this.

### How we know it is right

Thirteen tests that used to skip now run and pass, including the five-node simulations with 30% packet loss. I also broke the merge four ways on purpose (adding instead of taking the larger, not updating the total, not relaying, accepting any window) and each was caught. In the real cluster, three gateways ended a run holding identical per-node counts, and over-admission fell from 21.6% at a 500 ms gossip interval to 0.8% at 10 ms.

### Rebuild it

Write `Merge` from memory. Then explain to an empty chair: why totals and not increments; why `<=`; why the window range check exists; and what "commutative, associative, idempotent" each protect against.

## Check yourself

- What two numbers define a token bucket, and what does each control?
- What is lazy refill, and what does it save?
- Why does the gossip backend support the sliding window but not the token bucket?
- In the Lua script, which single line makes a rejected request leave no trace, by *not* being reached?
- Where does the script get the time, and why?
- In `Merge`, what would go wrong with `+=` in place of "keep the larger"?
- Why does `Merge` accept the *next* window but not the one after?
