# 05. The memory backend: locks, shards and the janitor

File: [internal/limiter/memory/memory.go](../internal/limiter/memory/memory.go)

The algorithm in [04-sliding-window.md](04-sliding-window.md) is arithmetic. This note is about everything around it: keeping the data safe when thousands of requests arrive at once, and keeping memory from growing for ever.

## The problem: two requests at the same moment

A web server handles each request in its own goroutine, so many `Allow` calls run at the same time. Picture two of them for the same key, with 99 of 100 already used:

| Time | Goroutine A | Goroutine B |
| --- | --- | --- |
| 1 | reads `cur` = 99 | |
| 2 | | reads `cur` = 99 |
| 3 | 99 + 1 fits, so allow | |
| 4 | | 99 + 1 fits, so allow |
| 5 | writes `cur` = 100 | |
| 6 | | writes `cur` = 100 |

Both were allowed. 101 requests passed and the counter says 100. One update was lost.

This is a **data race**, and this exact shape is called **check-then-act**: read a value, decide, write, with someone else slipping in between the read and the write.

Remember this table. It is the same bug the Redis backend must avoid in Phase 2 (two *gateways* reading the same count from Redis), and the reason that backend uses a Lua script. It is the most likely interview question about this project.

## The fix: a mutex

A `sync.Mutex` is a lock that only one goroutine can hold.

```go
s.mu.Lock()
defer s.mu.Unlock()
// read, decide, write
```

Goroutine A takes the lock. B reaches `Lock()` and waits. A reads, decides, writes and unlocks. Only then does B read, and it sees 100 and rejects. The read, the decision and the write now happen as one unbreakable step. That property is called **atomicity**.

## The new problem: one lock for everyone

One mutex for the whole limiter is correct but slow. Every request for every client waits in the same queue, even though requests for different clients have nothing to do with each other.

## The fix: sharding

Split the data into 64 separate maps, each with its own lock:

```go
const numShards = 64

type Backend struct {
	clock  limiter.Clock
	shards [numShards]shard
	...
}

type shard struct {
	mu      sync.Mutex
	windows map[string]*window
	buckets map[string]*bucket
	_ [40]byte
}
```

A key always goes to the same shard. Two requests wait on each other only if their keys happen to share a shard, which for different clients is a 1 in 64 chance.

Requests for the *same* key still queue behind one lock. They must: that is what keeps the count exact.

### Choosing the shard: hashing

```go
func (b *Backend) shardFor(key string) *shard {
	return &b.shards[fnv1a(key)%numShards]
}
```

A **hash function** turns a string into a number, with two properties that matter here: the same string always gives the same number, and different strings give numbers that look random. `% 64` then reduces that number to a shard index from 0 to 63.

```go
func fnv1a(key string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return h
}
```

This is FNV-1a, a short and fast hash. For each byte: mix it in with XOR (`^=`), then multiply by a fixed prime. The two odd-looking numbers are the standard constants of the algorithm.

**Why write it by hand when Go has `hash/fnv`?** The library version needs a hasher object and a byte-slice copy of the string, and both allocate memory. This function runs on every single request, so avoiding those allocations is worth eight lines. A test (`TestFNV1aMatchesStandardLibrary`) checks that the hand-written version gives the same answers as the library.

Another test (`TestKeysSpreadOverAllShards`) puts 12,800 keys in and checks that every shard gets roughly 200. If the hash were bad and piled keys into a few shards, sharding would achieve nothing.

### The padding

```go
_ [40]byte
```

A field named `_` that nothing ever reads. It exists only to make the struct bigger.

A CPU does not fetch memory one byte at a time. It fetches 64-byte blocks called **cache lines**. A shard without padding is 24 bytes, so two or three shards would sit in one cache line. When one core locks shard 5, the other cores' copies of that whole line become stale and must be fetched again, even if they only wanted shard 6. Cores slow each other down while touching *different* data. This is called **false sharing**.

Padding each shard to exactly 64 bytes gives every shard its own cache line. `8 (mutex) + 8 (map) + 8 (map) + 40 (padding) = 64`. A test checks the size is 64, so if someone adds a field later, the test fails and reminds them to adjust the padding.

## The janitor: forgetting idle clients

Every client that ever sends a request leaves an entry in a map. Without cleanup, a gateway that has seen a million clients holds a million entries for ever, though most went quiet long ago.

Each entry carries an expiry time. For the sliding window it is two windows after the last write, the point after which the stored counts no longer affect any decision. A background goroutine, the **janitor**, deletes expired entries:

```go
func (b *Backend) janitor(interval time.Duration) {
	defer close(b.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.sweep(b.clock.Now())
		}
	}
}
```

Reading it:

- `time.NewTicker(interval)` sends a signal on `ticker.C` once per interval.
- The loop waits in `select` for one of two things: a tick (do a sweep) or the stop signal (exit).
- `defer close(b.done)` announces "I have exited" on the way out.

`sweep` walks the shards one at a time, locking each only while it is being cleaned, so request handling pauses on one shard at a time and never on all of them.

**A detail worth noticing.** The *ticker* runs on real time. But *whether a key has expired* is judged with the injected clock (`b.clock.Now()`). So a test can jump the fake clock forward two seconds and the key counts as expired at once. And because `sweep` takes the time as an argument, a test can call `b.sweep(...)` directly and never wait for a ticker.

**Deleting is safe.** An expired entry and a missing entry lead to exactly the same decision. The test `TestSweptKeyDecidesLikeUnsweptKey` proves it by deleting one of two identical keys and comparing the decisions. The janitor only frees memory; it never changes behaviour.

## Shutting down cleanly

```go
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		close(b.stop)
		<-b.done
	})
	return nil
}
```

Three small ideas:

1. **`close(b.stop)`** is the stop signal. The janitor's `select` sees it and returns.
2. **`<-b.done`** waits until the janitor has really exited. Without this, `Close` would return while the goroutine was still running. A goroutine that outlives its owner is called a **goroutine leak**.
3. **`closeOnce.Do(...)`** runs the function only the first time. Closing an already closed channel crashes a Go program, so without `sync.Once` a second `Close()` call would be fatal. With it, calling `Close` twice is harmless.

## The entry point: `Allow`

```go
func (b *Backend) Allow(_ context.Context, key string, l limiter.Limit, cost int) (limiter.Decision, error) {
	if l.Algorithm != limiter.TokenBucket && l.Algorithm != limiter.SlidingWindow {
		return limiter.Decision{}, limiter.ErrUnsupported
	}
	if cost < 1 || cost > l.Capacity() {
		return limiter.Decision{}, limiter.ErrInvalidCost
	}

	now := b.clock.Now()
	if l.Algorithm == limiter.TokenBucket {
		return b.allowTokenBucket(key, l, cost, now)
	}
	return b.allowSlidingWindow(key, l, cost, now), nil
}
```

Check the inputs, read the clock **once**, hand over to the right algorithm. Reading the clock once matters: if the code called `Now()` in several places, the time could change between the calls and the arithmetic would use two different "nows".

## The token bucket

[token_bucket.go](../internal/limiter/memory/token_bucket.go) has the same shape as `allowSlidingWindow`: find the shard, lock it, look up the key, do the arithmetic, store. The plan had reserved it for you to write; it was later handed to Fable. The arithmetic and the reasoning behind every line are in [16-the-three-core-pieces.md](16-the-three-core-pieces.md), with an exercise to rebuild it yourself.

## Check yourself

- Draw the two-goroutine table from memory and point at the exact moment the bug happens.
- What does "atomic" mean, and which three operations must be atomic in a limiter?
- Why 64 locks and not 1? Why not one lock per key?
- What is false sharing, and why does a field that nobody reads fix it?
- Why does `Close` wait on `done`? Why does it need `sync.Once`?
- The ticker uses real time but expiry uses the injected clock. What does that make possible in tests?
