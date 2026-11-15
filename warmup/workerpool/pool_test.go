package workerpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestEveryJobIsDoneOnce(t *testing.T) {
	jobs := make(chan int)
	go func() {
		defer close(jobs)
		for i := 1; i <= 100; i++ {
			jobs <- i
		}
	}()

	sum := 0
	for r := range Run(context.Background(), 4, jobs, func(_ context.Context, n int) int { return n * 2 }) {
		sum += r
	}
	if sum != 10100 { // 2 × (1 + ... + 100)
		t.Errorf("sum = %d, want 10100", sum)
	}
}

func TestNoMoreWorkersThanAskedFor(t *testing.T) {
	jobs := make(chan int, 50)
	for i := range 50 {
		jobs <- i
	}
	close(jobs)

	var running, peak atomic.Int32
	results := Run(context.Background(), 3, jobs, func(_ context.Context, n int) int {
		now := running.Add(1)
		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		running.Add(-1)
		return n
	})
	for range results {
	}
	if peak.Load() > 3 {
		t.Errorf("%d jobs ran at once, want at most 3", peak.Load())
	}
}

// Cancelling stops the pool even though jobs keep coming and nobody is
// reading results any more.
func TestCancelStopsThePool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := make(chan int)
	go func() {
		for i := 0; ; i++ {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	results := Run(ctx, 4, jobs, func(_ context.Context, n int) int { return n })
	<-results // one result proves it is working
	cancel()

	closed := make(chan struct{})
	go func() {
		for range results {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool did not stop within 5 s of cancel")
	}
}
