# 08. The Redis backend: one exact limit shared by many gateways

Files: [redis.go](../internal/limiter/redis/redis.go), [token_bucket.lua](../internal/limiter/redis/scripts/token_bucket.lua), [sliding_window.lua](../internal/limiter/redis/scripts/sliding_window.lua), [integration_test.go](../internal/limiter/redis/integration_test.go)

## The problem this solves

The memory backend keeps its counts inside one gateway process. Run three gateways and each has its own counts. A client limited to 100 per second can send 100 to each gateway: 300 in total. The limit is no longer the limit.

The fix is to keep the counts in one place that all gateways use. That place is **Redis**: a small, fast database that keeps everything in memory.

## Why not just GET and SET?

The obvious way to use Redis:

1. `GET` the count from Redis.
2. In Go: is there room? If yes, add one.
3. `SET` the new count.

This is wrong, and for the same reason as the two-goroutine table in [05-memory-backend.md](05-memory-backend.md). Two gateways both `GET` 99, both decide "room for one more", both `SET` 100. Two requests passed; the count went up by one.

In the memory backend a mutex fixed it. A mutex lives inside one process, so it cannot stop a *different gateway on a different machine*. Something else is needed.

**I measured how bad it is.** The test `TestNaiveBackendAdmitsMoreThanTheLimit` runs this wrong approach on purpose: three limiter instances, 10,000 requests, a limit of 100.

| Backend | Admitted | Limit |
| --- | --- | --- |
| GET, decide in Go, SET | 2,053 to 2,336 (varies each run) | 100 |
| Lua script | 100 (every run) | 100 |

More than twenty times the limit. The numbers are saved in [bench/results/phase2-naive-race.txt](../bench/results/phase2-naive-race.txt) for the report. This is the strongest single answer to "why Lua?" in an interview: not "it could race" but "I ran it, and it admitted 2,000 against a limit of 100".

## The fix: run the whole decision inside Redis

Redis can run a small program, written in the language **Lua**, that you send to it. Redis runs one command at a time, and a script counts as one command. While a script runs, nothing else in Redis runs.

So the read, the decision and the write happen inside Redis as one unbreakable step. That is atomicity again, this time provided by Redis and not by a mutex. And it takes one network round trip, where GET-then-SET took two.

## Reading `token_bucket.lua`

```lua
local rate     = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local cost     = tonumber(ARGV[3])
```

A script receives two lists: `KEYS` (the Redis keys it will touch) and `ARGV` (everything else). Lua lists start at 1, not 0. Everything arrives as text, so `tonumber` converts it.

```lua
local now = tonumber(ARGV[4])
if now == nil then
  local t = redis.call('TIME')
  now = t[1] * 1000 + math.floor(t[2] / 1000)
end
```

**Where does the time come from?** From Redis itself. `TIME` returns seconds and microseconds; this turns them into milliseconds.

This matters. If each gateway sent its own clock reading, three gateways with slightly different clocks would disagree about how many tokens had refilled. With Redis's clock there is one clock, so there is nothing to disagree about.

The optional fourth argument is for tests only. It is how the conformance suite's fake clock reaches inside Redis.

```lua
local state  = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
```

`redis.call` runs a normal Redis command from inside the script. The state is kept in a **hash**: one Redis key holding several named fields, here `tokens` and `ts`. `HMGET` reads several fields at once.

The middle of the script is the token bucket arithmetic from the plan. Then:

```lua
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity * 1000 / rate) + 1000)
```

`HSET` writes the fields back. `PEXPIRE` tells Redis to delete the key by itself after that many milliseconds. This does the job the janitor goroutine does in the memory backend, with no code of ours.

```lua
return {allowed, math.floor(tokens), retry_ms, reset_ms}
```

Four whole numbers. **Redis cuts the fraction off any number a script returns**, so returning `0.5` seconds would arrive as `0`. That is why everything is in milliseconds and rounded with `math.ceil` first.

## Reading `redis.go`

### Embedding the scripts

```go
//go:embed scripts/token_bucket.lua
var tokenBucketSrc string
```

`//go:embed` is an instruction to the compiler: read this file while building and put its contents in this variable. The Lua file ends up inside the compiled program. There is no file to find, and none to forget, when the gateway runs in a container.

### Sending the script efficiently

```go
tokenBucket = goredis.NewScript(tokenBucketSrc)
...
res, err := script.Run(ctx, b.rdb, []string{prefix + key}, args...).Int64Slice()
```

Sending the whole script text on every request would be wasteful. Redis keeps a cache of scripts it has seen, each under a fingerprint (a SHA-1 hash) of its text. `Script.Run` sends only the fingerprint. If Redis answers "I do not know that script", for example after a restart, `Run` sends the full text once and carries on. You get this for free from the library.

### Three settings that matter

```go
MaxRetries: -1,
ContextTimeoutEnabled: true,
ReadTimeout:  o.timeout,
WriteTimeout: o.timeout,
```

**No retries.** By default the library retries a failed command. For most commands that is helpful. Here it is dangerous: suppose the script ran and took a token, but the *reply* was lost on the network. A retry runs the script again and takes a second token for one request. An operation that is safe to repeat is called **idempotent**. This script is not, so it must not be retried.

**A deadline that is really enforced.** Each call gets 50 ms:

```go
ctx, cancel := context.WithTimeout(ctx, b.timeout)
defer cancel()
```

A slow Redis must not make every request to the gateway slow. After 50 ms the call gives up and returns an error, and the gateway applies its policy (`fail_open` or `fail_closed`). `ContextTimeoutEnabled` is needed because, by default, this library ignores the context's deadline on socket reads and looks only at its own timeouts. I found that in the library's documentation before relying on it, and then wrote a test to prove it (below).

### Turning four numbers into a `Decision`

```go
return limiter.Decision{
	Allowed:    res[0] == 1,
	Limit:      l.Rate,
	Remaining:  int(res[1]),
	RetryAfter: time.Duration(res[2]) * time.Millisecond,
	ResetAfter: time.Duration(res[3]) * time.Millisecond,
}, nil
```

This lives in its own small function, `decisionFrom`, so it can be tested with no Redis at all.

## `sliding_window.lua`

The plan had reserved this script for you; it was later handed to Fable and is walked through line by line in [16-the-three-core-pieces.md](16-the-three-core-pieces.md).

One guard remains in the backend from when the file was an empty stub:

```go
slidingWindowWritten = hasCode(slidingWindowSrc)
```

`hasCode` returns true if the file has any line that is not blank and not a comment. If the file were ever emptied, the backend would answer `ErrUnsupported` instead of sending Redis a script with nothing in it.

## The tests

### Two kinds, split by a build tag

`redis_test.go` needs no Redis and runs with plain `go test ./...`.

`integration_test.go` starts with:

```go
//go:build integration
```

A **build tag**. The file is ignored unless you ask for it:

```
go test -tags integration ./internal/limiter/redis/
```

Tests that need Docker are slower and need more setup, so they are kept out of the default run. `go test ./...` stays fast and works on any machine.

### Getting a Redis to test against

The tests use **testcontainers**: a library that starts a real Redis in Docker when the tests begin and removes it when they end. This is better than faking Redis, because a fake would not run Lua the way real Redis does, and the Lua is the thing under test.

If the environment variable `REDIS_ADDR` is set, the tests use that Redis instead. That is how they run inside a container under the race detector.

### What each test proves

| Test | What it proves |
| --- | --- |
| `TestConformance` | The Redis backend passes the *same* suite as the memory backend, so the two behave alike |
| `TestThreeInstancesAdmitExactlyTheLimit` | Three backends sharing one Redis admit exactly the limit under 10,000 concurrent requests |
| `TestNaiveBackendAdmitsMoreThanTheLimit` | The GET-then-SET approach really does over-admit, and by how much |
| `TestRedisOutageAndRecovery` | With Redis gone, `Allow` fails fast; with Redis back, the same backend works again unaided |

### Taking Redis away during a test

Stopping the Redis container mid-test has a catch: Docker may give it a different port when it starts again, and the backend would be left pointing at the old one.

So the test puts a tiny **TCP proxy** between the backend and Redis: about sixty lines of Go that forward bytes in both directions. The backend connects to the proxy. The test can then:

- **stop the proxy**: connections are refused, exactly like Redis being down;
- **start it again** on the same port: Redis is "back";
- **switch on "blackhole" mode**: the proxy accepts the connection and never answers, like a Redis that has frozen.

The last one is the nastiest failure in real systems. A refused connection fails instantly. A connection that is accepted and then goes silent makes the caller wait, and without a deadline it waits a very long time.

## Problems met

These are also logged in [07-problems-and-fixes.md](07-problems-and-fixes.md), numbers 11 to 13.

- **A test that passed for the wrong reason.** The first version of the "no reply" test reported a failure after 0.5 ms. A 200 ms deadline cannot expire in 0.5 ms. The error text said "connection aborted": the call had failed on an old connection the proxy had just cut, and never reached the silent one. The test was green without testing the deadline. I rewrote it to keep calling until one call actually waits, and to fail if none does. It now reports `gave up after 200.2ms with: i/o timeout`.
- **The package name clash.** Our package is called `redis` and so is the library's. Go lets an import be renamed: `goredis "github.com/redis/go-redis/v9"`.
- **The editor warning on `integration_test.go`.** The Go editor plugin ignores files behind a build tag unless told about the tag. `.vscode/settings.json` now passes `-tags=integration`.

## Check yourself

- Why can a mutex not fix the lost-update race between two gateways?
- What makes a Lua script atomic in Redis?
- Why does the script read the time from Redis and not take it from the gateway?
- Why must this script never be retried automatically? Use the word "idempotent".
- Why do the scripts return milliseconds and not seconds?
- What is the difference between a refused connection and a connection that never answers, and which is worse without a deadline?
- How could you tell, from the output alone, that the first "no reply" test was not testing what it claimed?
