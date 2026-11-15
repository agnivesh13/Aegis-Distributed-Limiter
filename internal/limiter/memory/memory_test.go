package memory_test

import (
	"testing"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/limitertest"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/memory"
)

func newBackend(_ *testing.T, clock limiter.Clock) limiter.Limiter {
	return memory.New(clock)
}

func TestConformance(t *testing.T) {
	limitertest.Run(t, newBackend) // both algorithms
}
