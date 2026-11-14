package memory

import (
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// bucket is one key's token bucket state. It is guarded by its shard's mutex.
type bucket struct {
	tokens  float64
	ts      time.Time // when tokens was last brought up to date
	expires time.Time // the janitor removes the key once the clock reaches this
}

// allowTokenBucket is the piece the plan reserves for you to write by hand.
// Until it exists the backend refuses the token bucket.
//
// What to build is "Token bucket: exact behaviour" in the plan, steps 1 to 6.
// allowSlidingWindow in this package shows the shape: take the shard from
// b.shardFor(key), lock it, read or create the entry in s.buckets, decide,
// store. Allow has already checked that cost is between 1 and the capacity.
//
// Three things the conformance suite will hold you to:
//   - Round RetryAfter and ResetAfter up, never down, so that a caller who
//     waits RetryAfter is always let in.
//   - Set expires to now plus capacity/rate (step 6); the janitor does the
//     rest.
//   - A rejected request still stores tokens and ts (step 4), but takes
//     nothing.
//
// To start: in memory_test.go change the suite call to
// limitertest.Run(t, newBackend) and watch the TokenBucket cases fail.
func (b *Backend) allowTokenBucket(key string, l limiter.Limit, cost int, now time.Time) (limiter.Decision, error) {
	return limiter.Decision{}, limiter.ErrUnsupported
}
