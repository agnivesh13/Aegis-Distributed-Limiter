// Package limiter defines the contract every rate-limit backend implements.
//
// The types here are the fixed "Limiter contract" from the project plan. The
// behaviour behind them is pinned down by the conformance suite in
// limitertest, which every backend must pass.
package limiter

import (
	"context"
	"errors"
	"time"
)

type Algorithm string

const (
	TokenBucket   Algorithm = "token_bucket"
	SlidingWindow Algorithm = "sliding_window"
)

// Limit allows Rate requests per Period for one key.
type Limit struct {
	Algorithm Algorithm
	Rate      int           // requests per Period
	Period    time.Duration // window length, or the time to refill Rate tokens
	Burst     int           // token bucket capacity; 0 means Rate
}

// Capacity is the most a single request may cost: Burst (or Rate when Burst
// is 0) for the token bucket, and Rate for the sliding window.
func (l Limit) Capacity() int {
	if l.Algorithm == TokenBucket && l.Burst > 0 {
		return l.Burst
	}
	return l.Rate
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	Limit      int           // Rate, echoed for response headers
	Remaining  int           // whole requests still available, never negative
	RetryAfter time.Duration // zero when Allowed
	ResetAfter time.Duration // time until the full limit is available again
}

type Limiter interface {
	// Allow charges cost against key and reports the decision.
	// A rejected request consumes nothing.
	// The error is non-nil only when the backend cannot decide.
	Allow(ctx context.Context, key string, limit Limit, cost int) (Decision, error)
	Close() error
}

// Clock is injected into every backend so tests control time.
type Clock interface {
	Now() time.Time
}

// SystemClock is the wall clock, used everywhere outside tests.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

var (
	ErrUnsupported = errors.New("limiter: algorithm not supported by this backend")
	ErrInvalidCost = errors.New("limiter: cost must be between 1 and the capacity")
)
