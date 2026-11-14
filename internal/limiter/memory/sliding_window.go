package memory

import (
	"math"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// window is one key's sliding window counter state. It is guarded by its
// shard's mutex.
type window struct {
	period  time.Duration // the Period the counts were taken under
	idx     int64         // window number of cur
	cur     int64         // requests admitted in window idx
	prev    int64         // requests admitted in window idx-1
	expires time.Time     // two windows after the last write
}

// allowSlidingWindow follows "Sliding window counter: exact behaviour" in the
// plan step by step. The arithmetic mirrors sliding_window.lua expression
// for expression, so both backends round the same way.
func (b *Backend) allowSlidingWindow(key string, l limiter.Limit, cost int, now time.Time) limiter.Decision {
	w := int64(l.Period)
	nowNs := now.UnixNano()
	idx := nowNs / w
	elapsed := nowNs - idx*w
	if elapsed < 0 { // a clock before 1970: make the division round down
		idx--
		elapsed += w
	}

	s := b.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	// Rotate the stored counts into the current window. This works on
	// copies: a rejected request must leave the stored state untouched.
	// Counts taken under a different Period are meaningless and dropped.
	st := s.windows[key]
	var cur, prev int64
	if st != nil && st.period == l.Period {
		switch st.idx {
		case idx:
			cur, prev = st.cur, st.prev
		case idx - 1:
			prev = st.cur
		}
	}

	limit := float64(l.Rate)
	c := float64(cost)
	estimate := float64(cur) + float64(prev)*(1-float64(elapsed)/float64(w))

	d := limiter.Decision{Limit: l.Rate}
	switch {
	case estimate+c <= limit:
		d.Allowed = true
		cur += int64(cost)
		estimate += c
		if st == nil {
			st = &window{}
			s.windows[key] = st
		}
		*st = window{period: l.Period, idx: idx, cur: cur, prev: prev, expires: now.Add(2 * l.Period)}
	case float64(cur)+c <= limit:
		// The current window has room; the previous window's weight is
		// what blocks the request. Wait until it has decayed enough.
		// prev is non-zero here: with prev == 0 the estimate equals cur.
		wait := float64(w)*(1-(limit-float64(cur)-c)/float64(prev)) - float64(elapsed)
		d.RetryAfter = max(1, time.Duration(math.Ceil(wait)))
	default:
		// The current window alone is too full. Its end is the earliest
		// the request could fit, so this wait is a lower bound.
		d.RetryAfter = time.Duration(w - elapsed)
	}

	d.Remaining = max(0, int(math.Floor(limit-estimate)))
	switch {
	case cur > 0:
		d.ResetAfter = time.Duration(2*w - elapsed)
	case prev > 0:
		d.ResetAfter = time.Duration(w - elapsed)
	}
	return d
}
