package limiter

import (
	"math"
	"time"
)

// The sliding window counter arithmetic lives here because two backends,
// memory and gossip, keep their counts differently but must decide alike.
// The Redis backend runs the same formulas in sliding_window.lua, written
// in the same order so all three round the same way.

// WindowAt returns the number of the window that now falls in, counting
// windows of length period from 1 January 1970, and how far into that
// window now is.
func WindowAt(now time.Time, period time.Duration) (idx int64, elapsed time.Duration) {
	w := int64(period)
	nowNs := now.UnixNano()
	idx = nowNs / w
	e := nowNs - idx*w
	if e < 0 { // a clock before 1970: make the division round down
		idx--
		e += w
	}
	return idx, time.Duration(e)
}

// SlidingWindowDecision applies "Sliding window counter: exact behaviour"
// from the plan to one key.
//
// cur and prev are the admitted counts for the current and the previous
// window, and elapsed is how far into the current window the request falls.
// The function changes nothing: when the decision is Allowed, the caller
// adds cost to its current-window count.
func SlidingWindowDecision(l Limit, cost int, cur, prev int64, elapsed time.Duration) Decision {
	w := float64(l.Period)
	e := float64(elapsed)
	limit := float64(l.Rate)
	c := float64(cost)

	// The previous window counts in proportion to how much of it the last
	// one period still covers: all of it at the start of the current
	// window, none of it by the end.
	estimate := float64(cur) + float64(prev)*(1-e/w)

	d := Decision{Limit: l.Rate}
	switch {
	case estimate+c <= limit:
		d.Allowed = true
		cur += int64(cost)
		estimate += c
	case float64(cur)+c <= limit:
		// The current window has room; the previous window's weight is
		// what blocks the request. Wait until it has decayed enough.
		// prev is non-zero here: with prev == 0 the estimate equals cur.
		wait := w*(1-(limit-float64(cur)-c)/float64(prev)) - e
		d.RetryAfter = max(1, time.Duration(math.Ceil(wait)))
	default:
		// The current window alone is too full. Its end is the earliest
		// the request could fit, so this wait is a lower bound.
		d.RetryAfter = l.Period - elapsed
	}

	d.Remaining = max(0, int(math.Floor(limit-estimate)))
	switch {
	case cur > 0:
		d.ResetAfter = 2*l.Period - elapsed
	case prev > 0:
		d.ResetAfter = l.Period - elapsed
	}
	return d
}
