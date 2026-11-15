// Package memory is the single-node limiter backend. It keeps all state in
// process memory, so it is exact on one gateway and shares nothing between
// gateways.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// numShards is how many independently locked maps the keys are spread over,
// so requests for different keys rarely wait on the same mutex.
const numShards = 64

const defaultJanitorInterval = time.Second

// Backend implements limiter.Limiter in memory.
type Backend struct {
	clock  limiter.Clock
	shards [numShards]shard

	stop      chan struct{} // closed by Close to stop the janitor
	done      chan struct{} // closed by the janitor when it has stopped
	closeOnce sync.Once
}

// shard is one slice of the keyspace. Each algorithm has its own map, the
// same way the Redis backend gives each its own key prefix.
type shard struct {
	mu      sync.Mutex
	windows map[string]*window // sliding window state
	buckets map[string]*bucket // token bucket state

	// Pad the shard to 64 bytes so two shards never share a cache line;
	// without it, locking one shard slows its neighbours down.
	_ [40]byte
}

// Option adjusts a Backend at construction.
type Option func(*options)

type options struct {
	janitorInterval time.Duration
}

// WithJanitorInterval sets how often expired keys are removed. The default
// is one second.
func WithJanitorInterval(d time.Duration) Option {
	return func(o *options) { o.janitorInterval = d }
}

// New returns a Backend that reads the time from clock and starts its
// janitor goroutine. Call Close to stop it.
func New(clock limiter.Clock, opts ...Option) *Backend {
	o := options{janitorInterval: defaultJanitorInterval}
	for _, opt := range opts {
		opt(&o)
	}

	b := &Backend{
		clock: clock,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	for i := range b.shards {
		b.shards[i].windows = make(map[string]*window)
		b.shards[i].buckets = make(map[string]*bucket)
	}
	go b.janitor(o.janitorInterval)
	return b
}

// Allow never fails for a supported algorithm and a valid cost: memory can
// always decide, so the context is not consulted.
func (b *Backend) Allow(_ context.Context, key string, l limiter.Limit, cost int) (limiter.Decision, error) {
	if l.Algorithm != limiter.TokenBucket && l.Algorithm != limiter.SlidingWindow {
		return limiter.Decision{}, limiter.ErrUnsupported
	}
	if cost < 1 || cost > l.Capacity() {
		return limiter.Decision{}, limiter.ErrInvalidCost
	}

	now := b.clock.Now()
	if l.Algorithm == limiter.TokenBucket {
		return b.allowTokenBucket(key, l, cost, now), nil
	}
	return b.allowSlidingWindow(key, l, cost, now), nil
}

// Close stops the janitor. It is safe to call more than once. Allow keeps
// working afterwards; expired keys are just no longer removed.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		close(b.stop)
		<-b.done
	})
	return nil
}

// Len reports how many keys currently hold state, for the
// limiter_tracked_keys gauge.
func (b *Backend) Len() int {
	n := 0
	for i := range b.shards {
		s := &b.shards[i]
		s.mu.Lock()
		n += len(s.windows) + len(s.buckets)
		s.mu.Unlock()
	}
	return n
}

// shardFor picks a key's shard by the FNV-1a hash of the key. The hash is
// written out here because hash/fnv would allocate on every request.
func (b *Backend) shardFor(key string) *shard {
	return &b.shards[fnv1a(key)%numShards]
}

func fnv1a(key string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return h
}

// janitor removes expired keys every interval until Close. The ticker runs
// on real time; what counts as expired is judged by the injected clock.
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

// sweep deletes every key whose state has expired by now. An expired key
// and a missing key give the same decisions, so removal only frees memory.
func (b *Backend) sweep(now time.Time) {
	for i := range b.shards {
		s := &b.shards[i]
		s.mu.Lock()
		for key, w := range s.windows {
			if !now.Before(w.expires) {
				delete(s.windows, key)
			}
		}
		for key, bk := range s.buckets {
			if !now.Before(bk.expires) {
				delete(s.buckets, key)
			}
		}
		s.mu.Unlock()
	}
}
