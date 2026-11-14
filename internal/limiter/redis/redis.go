// Package redis is the exact, shared limiter backend. Every gateway sends
// each decision to one Redis, where a Lua script reads the state, decides and
// writes in a single atomic step.
package redis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

//go:embed scripts/token_bucket.lua
var tokenBucketSrc string

//go:embed scripts/sliding_window.lua
var slidingWindowSrc string

var (
	tokenBucket   = goredis.NewScript(tokenBucketSrc)
	slidingWindow = goredis.NewScript(slidingWindowSrc)

	// slidingWindowWritten is false while sliding_window.lua is still the
	// comment-only stub. The backend then refuses that algorithm.
	slidingWindowWritten = hasCode(slidingWindowSrc)
)

// ErrInvalidPeriod is returned for a Period the scripts cannot represent.
// They count in whole milliseconds.
var ErrInvalidPeriod = errors.New("limiter/redis: period must be a whole number of milliseconds, at least 1")

const (
	defaultPoolSize = 64
	defaultTimeout  = 50 * time.Millisecond
)

// Backend implements limiter.Limiter on one Redis instance.
type Backend struct {
	rdb     *goredis.Client
	timeout time.Duration

	// clock is nil in production: the scripts read the time from Redis.
	// Tests set it to drive the scripts through their now_ms argument.
	clock limiter.Clock
}

// Option adjusts a Backend at construction.
type Option func(*options)

type options struct {
	poolSize int
	timeout  time.Duration
	clock    limiter.Clock
}

// WithPoolSize sets how many connections to Redis the backend may hold.
func WithPoolSize(n int) Option { return func(o *options) { o.poolSize = n } }

// WithTimeout sets the deadline for one Allow call.
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithClock makes the scripts use this clock in place of Redis's own time.
// It exists for tests only.
func WithClock(c limiter.Clock) Option { return func(o *options) { o.clock = c } }

// New returns a Backend for the Redis at addr. It does not connect yet; the
// first call does.
func New(addr string, opts ...Option) *Backend {
	o := options{poolSize: defaultPoolSize, timeout: defaultTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		PoolSize: o.poolSize,

		// Never retry. If a script ran but its reply was lost, running it
		// again would charge the request twice.
		MaxRetries: -1,

		// Make the per-call deadline cut a slow read or write short. By
		// default go-redis looks only at its own socket timeouts.
		ContextTimeoutEnabled: true,
		ReadTimeout:           o.timeout,
		WriteTimeout:          o.timeout,
	})
	return &Backend{rdb: rdb, timeout: o.timeout, clock: o.clock}
}

// Allow runs the script for the limit's algorithm. It returns an error when
// Redis does not answer within the timeout; the caller's on_error policy
// decides what happens to the request then.
func (b *Backend) Allow(ctx context.Context, key string, l limiter.Limit, cost int) (limiter.Decision, error) {
	switch {
	case l.Algorithm == limiter.TokenBucket:
	case l.Algorithm == limiter.SlidingWindow && slidingWindowWritten:
	default:
		return limiter.Decision{}, limiter.ErrUnsupported
	}
	if cost < 1 || cost > l.Capacity() {
		return limiter.Decision{}, limiter.ErrInvalidCost
	}
	if l.Period < time.Millisecond || l.Period%time.Millisecond != 0 {
		return limiter.Decision{}, ErrInvalidPeriod
	}

	var (
		script *goredis.Script
		prefix string
		args   []any
	)
	if l.Algorithm == limiter.TokenBucket {
		script, prefix = tokenBucket, "rl:tb:"
		ratePerSec := float64(l.Rate) / l.Period.Seconds()
		args = []any{ratePerSec, l.Capacity(), cost}
	} else {
		script, prefix = slidingWindow, "rl:sw:"
		args = []any{l.Rate, l.Period.Milliseconds(), cost}
	}
	if b.clock != nil {
		args = append(args, b.clock.Now().UnixMilli())
	}

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	res, err := script.Run(ctx, b.rdb, []string{prefix + key}, args...).Int64Slice()
	if err != nil {
		return limiter.Decision{}, fmt.Errorf("limiter/redis: %w", err)
	}
	return decisionFrom(res, l)
}

// decisionFrom maps a script's reply, {allowed, remaining, retry_after_ms,
// reset_after_ms}, onto a Decision.
func decisionFrom(res []int64, l limiter.Limit) (limiter.Decision, error) {
	if len(res) != 4 {
		return limiter.Decision{}, fmt.Errorf("limiter/redis: script returned %d values, want 4", len(res))
	}
	return limiter.Decision{
		Allowed:    res[0] == 1,
		Limit:      l.Rate,
		Remaining:  int(res[1]),
		RetryAfter: time.Duration(res[2]) * time.Millisecond,
		ResetAfter: time.Duration(res[3]) * time.Millisecond,
	}, nil
}

// Ping reports whether Redis answers, for the gateway's /readyz.
func (b *Backend) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	if err := b.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("limiter/redis: %w", err)
	}
	return nil
}

// Close releases the connections to Redis.
func (b *Backend) Close() error {
	return b.rdb.Close()
}

// hasCode reports whether a Lua source holds anything besides blank lines
// and comments.
func hasCode(src string) bool {
	for line := range strings.Lines(src) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return true
		}
	}
	return false
}
