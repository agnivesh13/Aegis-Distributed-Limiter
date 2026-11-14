package gossip

// Merge folds one entry received from another node into this store. It
// reports whether the entry told this node something new.
//
// This is the piece the plan reserves for you to write by hand. Until it is
// written it does nothing, so nodes never learn from each other, and the
// tests that need it skip themselves with a message saying so.
//
// What to build is "Merge (on receiving an entry)" in PLAN.md:
//
//  1. Ignore the entry if its window is older than this node's previous
//     window, or more than one window ahead of its current one.
//  2. If e.Count is greater than the count stored for e.Node: raise the
//     window's total by the difference, store e.Count, and mark the count
//     dirty for s.retransmit rounds so the news is passed on.
//  3. Otherwise do nothing.
//
// What is already there for you to use (see store.go):
//
//   - s.shardFor(e.Key) gives the shard; lock sh.mu before touching it.
//   - sh.keys[e.Key] is the key's *keyState, or nil if the key is new.
//     Allow shows how a keyState is created.
//   - ks.window(e.Idx) gives the window's counts, creating them if needed.
//     Its fields are perNode, total and dirty.
//   - limiter.WindowAt(s.clock.Now(), period) gives this node's current
//     window number. The period is e.WindowMs milliseconds.
//
// Things to think through before you write it:
//
//   - Why "greater than" and not "add"? What would happen to the total if
//     the same packet arrived twice?
//   - An entry about this node itself (e.Node == s.self) follows the same
//     rule. When can that happen, and why is it useful?
//   - What should happen when e.WindowMs differs from the window length
//     this node already holds for the key?
//
// The tests in merge_test.go check that merging is order-independent and
// that merging the same entry twice changes nothing.
func (s *Store) Merge(e Entry) (changed bool) {
	return false
}
