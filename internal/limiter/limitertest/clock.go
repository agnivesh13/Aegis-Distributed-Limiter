// Package limitertest holds the fake clock and the conformance suite that
// every limiter backend must pass.
package limitertest

import (
	"sync"
	"time"
)

// FakeClock is a limiter.Clock that moves only when a test moves it.
// It is safe for concurrent use.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock by d. A negative d moves it backwards, which lets
// tests check how a backend copes with a clock that steps back.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set jumps the clock to t.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
