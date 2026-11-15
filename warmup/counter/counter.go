// Package counter is warm-up exercise 3: one counter shared by many
// goroutines, written three ways. Only two of them are correct.
package counter

import (
	"sync"
	"sync/atomic"
)

// Counter is anything that can be incremented and read.
type Counter interface {
	Inc()
	Value() int64
}

// Unprotected is wrong on purpose. n++ is three steps (read, add, write);
// two goroutines can read the same value, and one increment is lost. This
// is the same lost update as the check-then-act race in a limiter.
type Unprotected struct{ n int64 }

func (c *Unprotected) Inc()         { c.n++ }
func (c *Unprotected) Value() int64 { return c.n }

// WithMutex lets one goroutine at a time into the read-add-write.
type WithMutex struct {
	mu sync.Mutex
	n  int64
}

func (c *WithMutex) Inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *WithMutex) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// WithAtomic asks the CPU to do the read-add-write as one instruction. For
// a single number it is the fastest correct choice; a mutex is needed as
// soon as several values must change together.
type WithAtomic struct{ n atomic.Int64 }

func (c *WithAtomic) Inc()         { c.n.Add(1) }
func (c *WithAtomic) Value() int64 { return c.n.Load() }

// Hammer increments c from 100 goroutines, 1,000 times each.
func Hammer(c Counter) {
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			for range 1000 {
				c.Inc()
			}
		})
	}
	wg.Wait()
}
