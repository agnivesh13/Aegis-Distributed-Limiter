package gossip

import (
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// Merge folds one entry received from another node into this store. It
// reports whether the entry told this node something new. It follows
// "Merge (on receiving an entry)" in the plan.
//
// An entry says "node N has admitted Count requests for this key in this
// window, in total". A node's count only ever grows, so the larger of two
// reports is always the newer one, and keeping the larger is all a merge
// has to do. That makes merging safe against everything the network does:
//
//   - a duplicate packet: the count is not larger, so nothing changes;
//   - a late, older packet: the count is smaller, so nothing changes;
//   - a lost packet: the next report carries a larger total anyway.
//
// Adding the reported amounts instead would count a duplicate twice.
//
// The rule applies to this node's own count too. After a restart it holds
// nothing, and its peers hand its own earlier count back to it.
func (s *Store) Merge(e Entry) (changed bool) {
	if e.WindowMs <= 0 {
		return false
	}

	// 1. Only the previous, current and next windows are accepted. Older
	// ones can no longer affect a decision. "Next" allows for a peer whose
	// clock is slightly ahead; anything further is a broken clock or a
	// corrupt packet, and storing it would waste memory.
	idxNow, _ := limiter.WindowAt(s.clock.Now(), time.Duration(e.WindowMs)*time.Millisecond)
	if e.Idx < idxNow-1 || e.Idx > idxNow+1 {
		return false
	}

	sh := s.shardFor(e.Key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	ks := sh.keys[e.Key]
	if ks == nil {
		ks = &keyState{windowMs: e.WindowMs, windows: make(map[int64]*windowCounts)}
		sh.keys[e.Key] = ks
	} else if ks.windowMs != e.WindowMs {
		// The peer counts this key in windows of another length (a
		// config change part-way through a rollout). Window numbers of
		// different lengths cannot be compared, so the entry is ignored.
		return false
	}

	// 2. Keep the higher count. total is the cached sum of perNode, so it
	// rises by exactly the difference.
	wc := ks.window(e.Idx)
	old := wc.perNode[e.Node]
	if e.Count <= old {
		return false // 3. nothing new
	}
	wc.total += e.Count - old
	wc.perNode[e.Node] = e.Count

	// News is passed on: mark the count to be sent in the coming rounds,
	// so it reaches peers the original sender did not pick.
	wc.dirty[e.Node] = s.retransmit
	return true
}
