package limiter

import (
	"testing"
	"time"
)

func TestWindowAt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		now         time.Time
		period      time.Duration
		wantIdx     int64
		wantElapsed time.Duration
	}{
		{"on a boundary", time.Unix(1_700_000_000, 0), time.Second, 1_700_000_000, 0},
		{"a quarter in", time.Unix(1_700_000_000, 250_000_000), time.Second, 1_700_000_000, 250 * time.Millisecond},
		{"last nanosecond", time.Unix(1_700_000_000, 999_999_999), time.Second, 1_700_000_000, time.Second - 1},
		{"ten second windows", time.Unix(1_700_000_007, 0), 10 * time.Second, 170_000_000, 7 * time.Second},
		{"before 1970 rounds down", time.Unix(-1, 750_000_000), time.Second, -1, 750 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, elapsed := WindowAt(tc.now, tc.period)
			if idx != tc.wantIdx || elapsed != tc.wantElapsed {
				t.Errorf("WindowAt = (%d, %v), want (%d, %v)", idx, elapsed, tc.wantIdx, tc.wantElapsed)
			}
		})
	}
}

// The gossip backend can hold more than the limit in a window, because
// counts from other nodes arrive after the fact. The decision must stay
// sane there: reject, never report a negative Remaining.
func TestSlidingWindowDecisionAboveTheLimit(t *testing.T) {
	l := Limit{Algorithm: SlidingWindow, Rate: 100, Period: time.Second}
	d := SlidingWindowDecision(l, 1, 130, 0, 400*time.Millisecond)
	want := Decision{Limit: 100, RetryAfter: 600 * time.Millisecond, ResetAfter: 1600 * time.Millisecond}
	if d != want {
		t.Errorf("got %+v, want %+v", d, want)
	}
}
