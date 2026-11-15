//go:build racedemo

package counter

import "testing"

// This test exists to fail. It is behind a build tag so the normal test
// run and CI stay green. Run it to watch the race detector catch the bug:
//
//	go test -race -tags racedemo ./warmup/counter/
//
// Without -race it usually just reports a count below 100000.
func TestUnprotectedCounterLosesUpdates(t *testing.T) {
	c := &Unprotected{}
	Hammer(c)
	if got := c.Value(); got != 100_000 {
		t.Errorf("counted %d, want 100000: %d increments were lost", got, 100_000-got)
	}
}
