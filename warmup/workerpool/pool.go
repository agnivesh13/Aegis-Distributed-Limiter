// Package workerpool is warm-up exercise 2: a fixed number of goroutines
// share a queue of jobs, and the whole thing stops when its context is
// cancelled.
package workerpool

import (
	"context"
	"sync"
)

// Run starts `workers` goroutines. Each takes jobs from the jobs channel,
// applies do, and sends the result on the returned channel. The results
// channel is closed once every worker has stopped, which happens when jobs
// is closed and drained, or when ctx is cancelled.
func Run[J, R any](ctx context.Context, workers int, jobs <-chan J, do func(context.Context, J) R) <-chan R {
	results := make(chan R)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return // jobs closed: nothing left to do
					}
					result := do(ctx, job)
					// The receiver may have gone away, so sending must
					// also give up on cancel, or the worker would block
					// here for ever: a goroutine leak.
					select {
					case results <- result:
					case <-ctx.Done():
						return
					}
				}
			}
		})
	}
	// Close results only after every worker is done; closing earlier
	// would make a worker panic by sending on a closed channel.
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}
