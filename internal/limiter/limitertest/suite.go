package limitertest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agnivesh13/ratelimit-gateway/internal/limiter"
)

// Factory builds a fresh, empty limiter that reads the time from clock.
// The suite calls it once per case, so a backend with shared storage (Redis)
// must hand back a clean keyspace each time.
type Factory func(t *testing.T, clock limiter.Clock) limiter.Limiter

// epoch is where every case starts its clock. It is a whole multiple of
// every Period the suite uses, so each case begins exactly on a window
// boundary and the sliding-window arithmetic below is exact.
var epoch = time.Unix(1_700_000_000, 0)

// tolerance is how far above the exact value a returned duration may be.
// Backends round durations up (Redis works in whole milliseconds) so that
// waiting the returned time is always enough; a value below the exact one
// is a bug.
const tolerance = time.Millisecond

// Run checks a backend against the limiter contract.
//
// algorithms lists what the backend implements; leave it empty for both.
// Any algorithm left out must be refused with limiter.ErrUnsupported.
func Run(t *testing.T, newLimiter Factory, algorithms ...limiter.Algorithm) {
	t.Helper()
	if len(algorithms) == 0 {
		algorithms = []limiter.Algorithm{limiter.TokenBucket, limiter.SlidingWindow}
	}
	supported := make(map[limiter.Algorithm]bool, len(algorithms))
	for _, a := range algorithms {
		supported[a] = true
	}

	if supported[limiter.TokenBucket] {
		t.Run("TokenBucket", func(t *testing.T) { runTokenBucket(t, newLimiter) })
	}
	if supported[limiter.SlidingWindow] {
		t.Run("SlidingWindow", func(t *testing.T) { runSlidingWindow(t, newLimiter) })
	}
	for _, a := range algorithms {
		t.Run("Common/"+string(a), func(t *testing.T) { runCommon(t, newLimiter, a) })
	}
	t.Run("Unsupported", func(t *testing.T) { runUnsupported(t, newLimiter, supported) })
}

// The limits the cases use. Token bucket: 10 tokens per second, capacity 20,
// so one token takes 100 ms. Sliding window: 100 requests per 1 s window.
var (
	bucketLimit = limiter.Limit{Algorithm: limiter.TokenBucket, Rate: 10, Period: time.Second, Burst: 20}
	windowLimit = limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 100, Period: time.Second}
)

func limitFor(a limiter.Algorithm) limiter.Limit {
	if a == limiter.TokenBucket {
		return bucketLimit
	}
	return windowLimit
}

func runTokenBucket(t *testing.T, newLimiter Factory) {
	l := bucketLimit
	const key = "route:client"

	t.Run("FullBucketAllowsCapacity", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		for i := range 20 {
			d := h.allow(key, l, 1)
			if !d.Allowed {
				t.Fatalf("request %d rejected, want 20 allowed", i+1)
			}
			if want := 19 - i; d.Remaining != want {
				t.Errorf("request %d: Remaining = %d, want %d", i+1, d.Remaining, want)
			}
			if i == 0 {
				near(t, "ResetAfter after one request", d.ResetAfter, 100*time.Millisecond)
			}
		}
		d := h.allow(key, l, 1)
		if d.Allowed {
			t.Fatal("request 21 allowed, want rejected")
		}
		if d.Remaining != 0 {
			t.Errorf("Remaining = %d, want 0", d.Remaining)
		}
		near(t, "RetryAfter", d.RetryAfter, 100*time.Millisecond)
		near(t, "ResetAfter", d.ResetAfter, 2*time.Second)
	})

	t.Run("BurstZeroMeansRate", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		noBurst := limiter.Limit{Algorithm: limiter.TokenBucket, Rate: 10, Period: time.Second}
		h.exactly(key, noBurst, 1, 10)
	})

	t.Run("RefillsOneTokenPerInterval", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.exactly("route:early", l, 1, 20)
		h.exactly("route:ontime", l, 1, 20)

		h.clock.Advance(99 * time.Millisecond)
		if d := h.allow("route:early", l, 1); d.Allowed {
			t.Error("allowed 99 ms after draining, want rejected until 100 ms")
		}
		h.clock.Advance(time.Millisecond)
		h.exactly("route:ontime", l, 1, 1)
	})

	t.Run("NeverExceedsCapacity", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.allowN(key, l, 1, 5)
		h.clock.Advance(time.Hour)
		h.exactly(key, l, 1, 20)
	})

	t.Run("CostConsumesTokens", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		d := h.allow(key, l, 5)
		if !d.Allowed || d.Remaining != 15 {
			t.Fatalf("cost 5 on a full bucket: Allowed = %v, Remaining = %d; want true, 15", d.Allowed, d.Remaining)
		}
		h.exactly(key, l, 5, 3)
	})

	t.Run("RejectedConsumesNothing", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.allowN(key, l, 17, 1)
		d := h.allow(key, l, 5)
		if d.Allowed || d.Remaining != 3 {
			t.Fatalf("cost 5 with 3 tokens: Allowed = %v, Remaining = %d; want false, 3", d.Allowed, d.Remaining)
		}
		h.exactly(key, l, 1, 3)
	})

	t.Run("RetryAfterIsTimeUntilEnoughTokens", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.exactly("route:waits", l, 1, 20)
		h.exactly("route:early", l, 1, 20)

		d := h.allow("route:waits", l, 5)
		if d.Allowed {
			t.Fatal("cost 5 on an empty bucket allowed")
		}
		near(t, "RetryAfter", d.RetryAfter, 500*time.Millisecond)

		h.clock.Advance(499 * time.Millisecond)
		if h.allow("route:early", l, 5).Allowed {
			t.Error("cost 5 allowed 499 ms after draining, want rejected until 500 ms")
		}
		h.clock.Advance(d.RetryAfter - 499*time.Millisecond)
		if !h.allow("route:waits", l, 5).Allowed {
			t.Error("cost 5 rejected after waiting RetryAfter")
		}
	})

	t.Run("ClockSteppingBackAddsNoTokens", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.exactly(key, l, 1, 20)
		h.clock.Advance(-500 * time.Millisecond)
		d := h.allow(key, l, 1)
		if d.Allowed || d.Remaining != 0 {
			t.Errorf("after the clock stepped back: Allowed = %v, Remaining = %d; want false, 0", d.Allowed, d.Remaining)
		}
	})
}

func runSlidingWindow(t *testing.T, newLimiter Factory) {
	l := windowLimit
	const key = "route:client"

	t.Run("AllowsLimitInOneWindow", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.clock.Advance(250 * time.Millisecond)
		for i := range 100 {
			d := h.allow(key, l, 1)
			if !d.Allowed {
				t.Fatalf("request %d rejected, want 100 allowed", i+1)
			}
			if want := 99 - i; d.Remaining != want {
				t.Errorf("request %d: Remaining = %d, want %d", i+1, d.Remaining, want)
			}
		}
		d := h.allow(key, l, 1)
		if d.Allowed {
			t.Fatal("request 101 allowed, want rejected")
		}
		if d.Remaining != 0 {
			t.Errorf("Remaining = %d, want 0", d.Remaining)
		}
		// The current window is full, so the wait runs to its end; its
		// count then weighs on the whole of the next window too.
		near(t, "RetryAfter", d.RetryAfter, 750*time.Millisecond)
		near(t, "ResetAfter", d.ResetAfter, 1750*time.Millisecond)
	})

	// A full previous window weighs on the current one in proportion to how
	// much of the current window is still to come.
	for _, tc := range []struct {
		name   string
		offset time.Duration
		want   int
	}{
		{"StartOfNextWindow", 0, 0},
		{"QuarterThroughNextWindow", 250 * time.Millisecond, 25},
		{"HalfThroughNextWindow", 500 * time.Millisecond, 50},
		{"ThreeQuartersThroughNextWindow", 750 * time.Millisecond, 75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newLimiter)
			h.exactly(key, l, 1, 100)
			h.clock.Advance(time.Second + tc.offset)
			d := h.exactly(key, l, 1, tc.want)
			// One more request fits once the previous window has lost
			// one more count of weight: 1/100 of the window.
			near(t, "RetryAfter", d.RetryAfter, 10*time.Millisecond)
			if tc.want == 0 {
				near(t, "ResetAfter", d.ResetAfter, time.Second)
			} else {
				near(t, "ResetAfter", d.ResetAfter, 2*time.Second-tc.offset)
			}
		})
	}

	t.Run("FullLimitAfterTwoIdleWindows", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.clock.Advance(250 * time.Millisecond)
		h.exactly(key, l, 1, 100)
		h.clock.Advance(2 * time.Second)
		h.exactly(key, l, 1, 100)
	})

	t.Run("RetryAfterWaitsForPreviousWindowToDecay", func(t *testing.T) {
		// Limit 8 keeps every fraction here exact in binary floating
		// point, so the 1 ms boundaries below are safe on any backend.
		l8 := limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 8, Period: time.Second}
		h := newHarness(t, newLimiter)
		h.exactly(key, l8, 1, 8)
		h.clock.Advance(1500 * time.Millisecond)

		// previous = 8 at half weight, so 4 fit. A fifth fits when the
		// previous window's weight falls to 3/8, at 625 ms: 125 ms away.
		d := h.exactly(key, l8, 1, 4)
		near(t, "RetryAfter", d.RetryAfter, 125*time.Millisecond)

		h.clock.Advance(124 * time.Millisecond)
		if h.allow(key, l8, 1).Allowed {
			t.Error("allowed 1 ms before RetryAfter elapsed")
		}
		h.clock.Advance(time.Millisecond)
		if !h.allow(key, l8, 1).Allowed {
			t.Error("rejected after waiting RetryAfter")
		}
	})

	t.Run("CostConsumesCount", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		d := h.allow(key, l, 5)
		if !d.Allowed || d.Remaining != 95 {
			t.Fatalf("cost 5 on an empty window: Allowed = %v, Remaining = %d; want true, 95", d.Allowed, d.Remaining)
		}
		h.exactly(key, l, 5, 19)
	})

	t.Run("RejectedConsumesNothing", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.allowN(key, l, 98, 1)
		d := h.allow(key, l, 5)
		if d.Allowed || d.Remaining != 2 {
			t.Fatalf("cost 5 with 2 left: Allowed = %v, Remaining = %d; want false, 2", d.Allowed, d.Remaining)
		}
		h.exactly(key, l, 1, 2)
	})
}

// runCommon holds the cases that read the same for both algorithms.
func runCommon(t *testing.T, newLimiter Factory, a limiter.Algorithm) {
	l := limitFor(a)
	capacity := l.Capacity()

	t.Run("KeysAreIndependent", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.exactly("route:alice", l, 1, capacity)
		h.exactly("route:bob", l, 1, capacity)
	})

	t.Run("InvalidCostChangesNothing", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		for _, cost := range []int{0, -1, capacity + 1} {
			_, err := h.lim.Allow(context.Background(), "route:client", l, cost)
			if !errors.Is(err, limiter.ErrInvalidCost) {
				t.Errorf("cost %d: err = %v, want ErrInvalidCost", cost, err)
			}
		}
		h.exactly("route:client", l, 1, capacity)
	})

	t.Run("ResetAfterRestoresFullLimit", func(t *testing.T) {
		h := newHarness(t, newLimiter)
		h.clock.Advance(250 * time.Millisecond)
		d := h.exactly("route:client", l, 1, capacity)
		h.clock.Advance(d.ResetAfter)
		h.exactly("route:client", l, 1, capacity)
	})

	t.Run("ConcurrentAdmitsExactlyCapacity", func(t *testing.T) {
		const goroutines, perGoroutine = 50, 200 // 10,000 requests, clock frozen
		h := newHarness(t, newLimiter)

		var (
			wg       sync.WaitGroup
			admitted atomic.Int64
			failed   atomic.Pointer[error]
		)
		for range goroutines {
			wg.Go(func() {
				for range perGoroutine {
					d, err := h.lim.Allow(context.Background(), "route:client", l, 1)
					if err != nil {
						failed.CompareAndSwap(nil, &err)
						return
					}
					if d.Allowed {
						admitted.Add(1)
					}
				}
			})
		}
		wg.Wait()

		if err := failed.Load(); err != nil {
			t.Fatalf("Allow: %v", *err)
		}
		if got := admitted.Load(); got != int64(capacity) {
			t.Errorf("admitted %d of %d concurrent requests, want exactly %d", got, goroutines*perGoroutine, capacity)
		}
	})
}

func runUnsupported(t *testing.T, newLimiter Factory, supported map[limiter.Algorithm]bool) {
	h := newHarness(t, newLimiter)
	refused := []limiter.Algorithm{"no_such_algorithm"}
	for _, a := range []limiter.Algorithm{limiter.TokenBucket, limiter.SlidingWindow} {
		if !supported[a] {
			refused = append(refused, a)
		}
	}
	for _, a := range refused {
		l := limitFor(a)
		l.Algorithm = a
		_, err := h.lim.Allow(context.Background(), "route:client", l, 1)
		if !errors.Is(err, limiter.ErrUnsupported) {
			t.Errorf("algorithm %q: err = %v, want ErrUnsupported", a, err)
		}
	}
}

// harness is one case's limiter and the clock that drives it.
type harness struct {
	t     *testing.T
	clock *FakeClock
	lim   limiter.Limiter
}

func newHarness(t *testing.T, newLimiter Factory) *harness {
	t.Helper()
	clock := NewFakeClock(epoch)
	lim := newLimiter(t, clock)
	t.Cleanup(func() {
		if err := lim.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return &harness{t: t, clock: clock, lim: lim}
}

// allow makes one request and checks what must hold for every decision.
func (h *harness) allow(key string, l limiter.Limit, cost int) limiter.Decision {
	h.t.Helper()
	d, err := h.lim.Allow(context.Background(), key, l, cost)
	if err != nil {
		h.t.Fatalf("Allow(%q, cost %d): %v", key, cost, err)
	}
	if d.Limit != l.Rate {
		h.t.Errorf("Limit = %d, want the rate %d", d.Limit, l.Rate)
	}
	if d.Remaining < 0 {
		h.t.Errorf("Remaining = %d, want it never negative", d.Remaining)
	}
	if d.ResetAfter < 0 {
		h.t.Errorf("ResetAfter = %v, want it never negative", d.ResetAfter)
	}
	if d.Allowed && d.RetryAfter != 0 {
		h.t.Errorf("RetryAfter = %v on an allowed request, want 0", d.RetryAfter)
	}
	if !d.Allowed && d.RetryAfter <= 0 {
		h.t.Errorf("RetryAfter = %v on a rejected request, want it positive", d.RetryAfter)
	}
	return d
}

// allowN makes n requests that must all be allowed.
func (h *harness) allowN(key string, l limiter.Limit, cost, n int) {
	h.t.Helper()
	for i := range n {
		if d := h.allow(key, l, cost); !d.Allowed {
			h.t.Fatalf("request %d of %d rejected, want allowed (%+v)", i+1, n, d)
		}
	}
}

// exactly checks that the next want requests are allowed and the one after
// them is rejected. It returns the rejecting decision.
func (h *harness) exactly(key string, l limiter.Limit, cost, want int) limiter.Decision {
	h.t.Helper()
	h.allowN(key, l, cost, want)
	d := h.allow(key, l, cost)
	if d.Allowed {
		h.t.Fatalf("request %d allowed, want only %d allowed", want+1, want)
	}
	return d
}

// near checks got against the exact value want, allowing it to be rounded up.
func near(t *testing.T, name string, got, want time.Duration) {
	t.Helper()
	if got < want || got > want+tolerance {
		t.Errorf("%s = %v, want %v (or up to %v more)", name, got, want, tolerance)
	}
}
