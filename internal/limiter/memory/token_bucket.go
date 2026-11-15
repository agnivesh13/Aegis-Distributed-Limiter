package memory

import (
	"math"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// bucket is one key's token bucket state. It is guarded by its shard's mutex.
type bucket struct {
	tokens  float64
	ts      time.Time // when tokens was last brought up to date
	expires time.Time // the janitor removes the key once the clock reaches this
}

// allowTokenBucket follows "Token bucket: exact behaviour" in the plan, step
// by step. The bucket refills lazily: nothing runs in the background, and
// each request works out how many tokens arrived since the last one.
func (b *Backend) allowTokenBucket(key string, l limiter.Limit, cost int, now time.Time) limiter.Decision {
	capacity := float64(l.Capacity())
	rate := float64(l.Rate) / l.Period.Seconds() // tokens per second
	c := float64(cost)

	s := b.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. A key seen for the first time starts with a full bucket.
	bk := s.buckets[key]
	if bk == nil {
		bk = &bucket{tokens: capacity, ts: now}
		s.buckets[key] = bk
	}

	// 2. Refill for the time that has passed, never above capacity. A
	// clock that stepped backwards counts as no time passing.
	elapsed := max(0, now.Sub(bk.ts).Seconds())
	bk.tokens = math.Min(capacity, bk.tokens+elapsed*rate)

	// 3. Take the tokens if there are enough; otherwise say how long
	// until there will be.
	d := limiter.Decision{Limit: l.Rate}
	if bk.tokens >= c {
		d.Allowed = true
		bk.tokens -= c
	} else {
		d.RetryAfter = max(1, ceilSeconds((c-bk.tokens)/rate))
	}

	// 4. The refilled count and the time are stored whether or not the
	// request was allowed. A rejected request took no tokens.
	bk.ts = now

	// 5. What the caller is told.
	d.Remaining = int(math.Floor(bk.tokens))
	d.ResetAfter = ceilSeconds((capacity - bk.tokens) / rate)

	// 6. After capacity/rate idle seconds the bucket would be full again,
	// which is exactly what a missing key means, so the key can go.
	bk.expires = now.Add(ceilSeconds(capacity / rate))
	return d
}

// ceilSeconds converts seconds to a Duration, rounding up to the next
// nanosecond so that a caller who waits the returned time has always
// waited long enough.
func ceilSeconds(s float64) time.Duration {
	return time.Duration(math.Ceil(s * float64(time.Second)))
}
