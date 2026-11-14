package memory

import (
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

// allowSlidingWindow keeps the counts; the arithmetic that turns them into
// a decision is limiter.SlidingWindowDecision, shared with the gossip
// backend.
func (b *Backend) allowSlidingWindow(key string, l limiter.Limit, cost int, now time.Time) limiter.Decision {
	idx, elapsed := limiter.WindowAt(now, l.Period)

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

	d := limiter.SlidingWindowDecision(l, cost, cur, prev, elapsed)
	if d.Allowed {
		if st == nil {
			st = &window{}
			s.windows[key] = st
		}
		*st = window{period: l.Period, idx: idx, cur: cur + int64(cost), prev: prev, expires: now.Add(2 * l.Period)}
	}
	return d
}
