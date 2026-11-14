// Package gossip is the approximate, Redis-free limiter backend. Each
// gateway decides every request from its own memory and tells the others
// what it admitted in the background.
package gossip

import (
	"sort"
	"sync"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

const numShards = 64

// Store holds this node's view of every node's admitted counts. It is pure
// state and arithmetic: no network, no goroutines. Node adds those.
//
// For each key and each window, the store keeps one count per node. A
// node only ever raises its own count, and every node keeps the highest
// value it has heard for each other node. A counter built this way is
// called a grow-only counter (G-Counter): the total is the sum over nodes,
// and two copies are reconciled by taking the larger value node by node.
type Store struct {
	self       string // this node's ID
	clock      limiter.Clock
	retransmit int // gossip rounds a changed count keeps being sent
	shards     [numShards]shard
}

type shard struct {
	mu   sync.Mutex
	keys map[string]*keyState

	_ [48]byte // pad to a 64-byte cache line, as in the memory backend
}

// keyState is one key's counts, guarded by its shard's mutex.
type keyState struct {
	windowMs int64                   // the window length the counts were taken under
	windows  map[int64]*windowCounts // window number -> counts
}

// windowCounts is one window's counts for one key.
type windowCounts struct {
	perNode map[string]uint64 // node ID -> requests that node admitted in this window
	total   uint64            // cached sum of perNode
	dirty   map[string]int    // node ID -> gossip rounds this count still has to be sent
}

// NewStore returns an empty store for the node called self.
func NewStore(self string, clock limiter.Clock, retransmit int) *Store {
	s := &Store{self: self, clock: clock, retransmit: retransmit}
	for i := range s.shards {
		s.shards[i].keys = make(map[string]*keyState)
	}
	return s
}

func (s *Store) shardFor(key string) *shard {
	h := uint32(2166136261) // FNV-1a, as in the memory backend
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return &s.shards[h%numShards]
}

// window returns the counts for window idx, creating them if needed.
func (ks *keyState) window(idx int64) *windowCounts {
	wc := ks.windows[idx]
	if wc == nil {
		wc = &windowCounts{perNode: make(map[string]uint64), dirty: make(map[string]int)}
		ks.windows[idx] = wc
	}
	return wc
}

// total returns the summed count for window idx, or 0 if there is none.
func (ks *keyState) total(idx int64) int64 {
	if wc := ks.windows[idx]; wc != nil {
		return int64(wc.total)
	}
	return 0
}

// dropOld forgets windows that can no longer affect a decision: anything
// before the previous window.
func (ks *keyState) dropOld(idxNow int64) {
	for idx := range ks.windows {
		if idx < idxNow-1 {
			delete(ks.windows, idx)
		}
	}
}

// Allow decides one request from local memory alone. It follows "Decision
// (request path, no network)" in the plan. The caller has already checked
// the algorithm, the cost and the period.
func (s *Store) Allow(key string, l limiter.Limit, cost int) limiter.Decision {
	now := s.clock.Now()
	idx, elapsed := limiter.WindowAt(now, l.Period)
	windowMs := l.Period.Milliseconds()

	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	ks := sh.keys[key]
	if ks != nil && ks.windowMs != windowMs {
		// Counts taken under another window length mean nothing now.
		delete(sh.keys, key)
		ks = nil
	}
	var cur, prev int64
	if ks != nil {
		ks.dropOld(idx)
		cur, prev = ks.total(idx), ks.total(idx-1)
	}

	d := limiter.SlidingWindowDecision(l, cost, cur, prev, elapsed)
	if d.Allowed {
		if ks == nil {
			ks = &keyState{windowMs: windowMs, windows: make(map[int64]*windowCounts)}
			sh.keys[key] = ks
		}
		wc := ks.window(idx)
		wc.perNode[s.self] += uint64(cost)
		wc.total += uint64(cost)
		wc.dirty[s.self] = s.retransmit
	}
	return d
}

// Dirty returns every count that still has gossip rounds left to be sent,
// and uses up one round for each. The gossip loop calls it once per round.
func (s *Store) Dirty() []Entry {
	var out []Entry
	s.eachLiveWindow(func(key string, ks *keyState, idx int64, wc *windowCounts) {
		for node, rounds := range wc.dirty {
			out = append(out, Entry{Key: key, WindowMs: ks.windowMs, Idx: idx, Node: node, Count: wc.perNode[node]})
			if rounds <= 1 {
				delete(wc.dirty, node)
			} else {
				wc.dirty[node] = rounds - 1
			}
		}
	})
	return out
}

// Snapshot returns every live count, for a full sync.
func (s *Store) Snapshot() []Entry {
	var out []Entry
	s.eachLiveWindow(func(key string, ks *keyState, idx int64, wc *windowCounts) {
		for node, count := range wc.perNode {
			out = append(out, Entry{Key: key, WindowMs: ks.windowMs, Idx: idx, Node: node, Count: count})
		}
	})
	return out
}

// eachLiveWindow calls f for every window that can still affect a decision,
// one shard at a time under that shard's lock.
func (s *Store) eachLiveWindow(f func(key string, ks *keyState, idx int64, wc *windowCounts)) {
	now := s.clock.Now()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for key, ks := range sh.keys {
			idxNow, _ := limiter.WindowAt(now, time.Duration(ks.windowMs)*time.Millisecond)
			for idx, wc := range ks.windows {
				if idx >= idxNow-1 {
					f(key, ks, idx, wc)
				}
			}
		}
		sh.mu.Unlock()
	}
}

// Sweep removes windows too old to matter and keys left with none. Removing
// them changes no decision; it only frees memory.
func (s *Store) Sweep() {
	now := s.clock.Now()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for key, ks := range sh.keys {
			idxNow, _ := limiter.WindowAt(now, time.Duration(ks.windowMs)*time.Millisecond)
			ks.dropOld(idxNow)
			if len(ks.windows) == 0 {
				delete(sh.keys, key)
			}
		}
		sh.mu.Unlock()
	}
}

// Len reports how many keys hold state, for the limiter_tracked_keys gauge.
func (s *Store) Len() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		n += len(sh.keys)
		sh.mu.Unlock()
	}
	return n
}

// WindowView is one window's counts as shown by /debug/limiter.
type WindowView struct {
	Idx     int64             `json:"idx"`
	Total   uint64            `json:"total"`
	PerNode map[string]uint64 `json:"per_node"`
}

// Counts returns this node's view of one key, oldest window first.
func (s *Store) Counts(key string) []WindowView {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	ks := sh.keys[key]
	if ks == nil {
		return nil
	}
	views := make([]WindowView, 0, len(ks.windows))
	for idx, wc := range ks.windows {
		perNode := make(map[string]uint64, len(wc.perNode))
		for node, count := range wc.perNode {
			perNode[node] = count
		}
		views = append(views, WindowView{Idx: idx, Total: wc.total, PerNode: perNode})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Idx < views[j].Idx })
	return views
}
