package counter

import "testing"

func TestMutexCounterIsExact(t *testing.T) {
	c := &WithMutex{}
	Hammer(c)
	if got := c.Value(); got != 100_000 {
		t.Errorf("counted %d, want 100000", got)
	}
}

func TestAtomicCounterIsExact(t *testing.T) {
	c := &WithAtomic{}
	Hammer(c)
	if got := c.Value(); got != 100_000 {
		t.Errorf("counted %d, want 100000", got)
	}
}
