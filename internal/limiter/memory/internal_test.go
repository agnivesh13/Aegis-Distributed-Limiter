package memory

import (
	"context"
	"fmt"
	"hash/fnv"
	"testing"
	"time"
	"unsafe"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/limitertest"
)

var (
	start = time.Unix(1_700_000_000, 0)
	limit = limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 100, Period: time.Second}
)

// newTestBackend returns a backend whose janitor never fires on its own, so
// each test decides when a sweep happens.
func newTestBackend(t *testing.T) (*Backend, *limitertest.FakeClock) {
	t.Helper()
	clock := limitertest.NewFakeClock(start)
	b := New(clock, WithJanitorInterval(time.Hour))
	t.Cleanup(func() { b.Close() })
	return b, clock
}

func mustAllow(t *testing.T, b *Backend, key string, l limiter.Limit) limiter.Decision {
	t.Helper()
	d, err := b.Allow(context.Background(), key, l, 1)
	if err != nil {
		t.Fatalf("Allow(%q): %v", key, err)
	}
	return d
}

// A key lives for two windows after its last write and is removed by the
// first sweep at or after that point.
func TestSweepRemovesExpiredKeys(t *testing.T) {
	b, clock := newTestBackend(t)
	mustAllow(t, b, "route:idle", limit)
	clock.Advance(time.Second)
	mustAllow(t, b, "route:busy", limit)

	clock.Advance(time.Second - time.Nanosecond)
	b.sweep(clock.Now())
	if got := b.Len(); got != 2 {
		t.Fatalf("just before the first key expires: Len = %d, want 2", got)
	}

	clock.Advance(time.Nanosecond)
	b.sweep(clock.Now())
	if got := b.Len(); got != 1 {
		t.Fatalf("after the first key expires: Len = %d, want 1", got)
	}

	clock.Advance(time.Second)
	b.sweep(clock.Now())
	if got := b.Len(); got != 0 {
		t.Fatalf("after both keys expire: Len = %d, want 0", got)
	}
}

// Removing an expired key must not change any decision: the counts it held
// were already too old to matter.
func TestSweptKeyDecidesLikeUnsweptKey(t *testing.T) {
	b, clock := newTestBackend(t)
	for range 100 {
		mustAllow(t, b, "route:swept", limit)
		mustAllow(t, b, "route:kept", limit)
	}
	clock.Advance(2 * time.Second)

	s := b.shardFor("route:swept")
	s.mu.Lock()
	delete(s.windows, "route:swept")
	s.mu.Unlock()

	swept := mustAllow(t, b, "route:swept", limit)
	kept := mustAllow(t, b, "route:kept", limit)
	if swept != kept {
		t.Errorf("swept key decided %+v, unswept key decided %+v; want the same", swept, kept)
	}
}

// The janitor goroutine runs sweeps by itself.
func TestJanitorSweepsOnItsOwn(t *testing.T) {
	clock := limitertest.NewFakeClock(start)
	b := New(clock, WithJanitorInterval(time.Millisecond))
	defer b.Close()

	mustAllow(t, b, "route:client", limit)
	clock.Advance(2 * time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for b.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("janitor did not remove the expired key within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCloseTwiceThenAllow(t *testing.T) {
	b := New(limitertest.NewFakeClock(start))
	if err := b.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if d := mustAllow(t, b, "route:client", limit); !d.Allowed {
		t.Error("Allow after Close rejected a first request")
	}
}

// Counts taken under one Period say nothing about another, so a changed
// Period starts the key afresh.
func TestChangedPeriodStartsAfresh(t *testing.T) {
	b, _ := newTestBackend(t)
	for range 100 {
		mustAllow(t, b, "route:client", limit)
	}
	if mustAllow(t, b, "route:client", limit).Allowed {
		t.Fatal("request 101 allowed under the original limit")
	}

	longer := limit
	longer.Period = 10 * time.Second
	if d := mustAllow(t, b, "route:client", longer); !d.Allowed || d.Remaining != 99 {
		t.Errorf("first request under a new Period: Allowed = %v, Remaining = %d; want true, 99", d.Allowed, d.Remaining)
	}
}

func TestShardFillsOneCacheLine(t *testing.T) {
	if got := unsafe.Sizeof(shard{}); got != 64 {
		t.Errorf("shard is %d bytes, want 64; adjust its padding", got)
	}
}

func TestFNV1aMatchesStandardLibrary(t *testing.T) {
	for _, key := range []string{"", "a", "route:client", "search:key-123", "api:10.0.0.7"} {
		h := fnv.New32a()
		h.Write([]byte(key))
		if got, want := fnv1a(key), h.Sum32(); got != want {
			t.Errorf("fnv1a(%q) = %d, want %d", key, got, want)
		}
	}
}

// With many keys every shard should be in use and none should hold far more
// than its share; otherwise the sharding is not spreading lock contention.
func TestKeysSpreadOverAllShards(t *testing.T) {
	b, _ := newTestBackend(t)
	const keys = 64 * 200
	for i := range keys {
		mustAllow(t, b, fmt.Sprintf("api:key-%d", i), limit)
	}
	for i := range b.shards {
		n := len(b.shards[i].windows)
		if n < 100 || n > 300 {
			t.Errorf("shard %d holds %d keys, want about 200", i, n)
		}
	}
}
