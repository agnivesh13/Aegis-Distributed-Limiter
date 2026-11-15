package memory_test

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/memory"
)

// BenchmarkAllow measures one Allow call: from one goroutine, then from
// every CPU at once on a single hot key and on 10,000 keys.
//
//	go test -run '^$' -bench BenchmarkAllow -benchmem ./internal/limiter/memory/
//
// The limit is generous, so almost every call is allowed and writes state:
// the slower of the two paths.
func BenchmarkAllow(b *testing.B) {
	for _, l := range []limiter.Limit{
		{Algorithm: limiter.SlidingWindow, Rate: 1_000_000_000, Period: time.Second},
		{Algorithm: limiter.TokenBucket, Rate: 1_000_000_000, Period: time.Second},
	} {
		b.Run(string(l.Algorithm), func(b *testing.B) {
			lim := memory.New(limiter.SystemClock{})
			defer lim.Close()
			if _, err := lim.Allow(context.Background(), "probe", l, 1); errors.Is(err, limiter.ErrUnsupported) {
				b.Skip("not implemented yet")
			}

			keys := make([]string, 10_000)
			for i := range keys {
				keys[i] = "api:key-" + strconv.Itoa(i)
			}

			b.Run("serial", func(b *testing.B) {
				for i := 0; b.Loop(); i++ {
					lim.Allow(context.Background(), keys[i%len(keys)], l, 1)
				}
			})
			b.Run("parallel/1key", func(b *testing.B) {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						lim.Allow(context.Background(), "api:hot", l, 1)
					}
				})
			})
			b.Run("parallel/10000keys", func(b *testing.B) {
				var next atomic.Uint64
				b.RunParallel(func(pb *testing.PB) {
					i := next.Add(7919) // each goroutine starts somewhere else
					for pb.Next() {
						i++
						lim.Allow(context.Background(), keys[i%uint64(len(keys))], l, 1)
					}
				})
			})
		})
	}
}
