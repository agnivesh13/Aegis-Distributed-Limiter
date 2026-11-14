package memory_test

import (
	"testing"

	"github.com/agnivesh13/ratelimit-gateway/internal/limiter"
	"github.com/agnivesh13/ratelimit-gateway/internal/limiter/limitertest"
	"github.com/agnivesh13/ratelimit-gateway/internal/limiter/memory"
)

func newBackend(_ *testing.T, clock limiter.Clock) limiter.Limiter {
	return memory.New(clock)
}

func TestConformance(t *testing.T) {
	// Only the sliding window is listed while the token bucket is unwritten.
	// Once allowTokenBucket exists, drop the last argument so the suite
	// checks both algorithms.
	limitertest.Run(t, newBackend, limiter.SlidingWindow)
}
