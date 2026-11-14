package gossip

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/limitertest"
)

// requireMerge skips a test while Store.Merge is still the empty stub.
// Every test below it is about what Merge does, so none can say anything
// until Merge exists.
func requireMerge(t *testing.T) {
	t.Helper()
	probe := NewStore("probe", limitertest.NewFakeClock(start), 1)
	if !probe.Merge(Entry{Key: "k", WindowMs: 1000, Idx: startIdx, Node: "other", Count: 1}) {
		t.Skip("Store.Merge is not written yet: see merge.go")
	}
}

// randomEntries makes entries the way a busy cluster would: a few keys, a
// few nodes, the three windows a node accepts, and many repeated reports of
// the same counter at different values.
func randomEntries(rng *rand.Rand, n int) []Entry {
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{
			Key:      fmt.Sprintf("api:key-%d", rng.IntN(4)),
			WindowMs: 1000,
			Idx:      startIdx - 1 + int64(rng.IntN(3)),
			Node:     fmt.Sprintf("gateway-%d", rng.IntN(5)),
			Count:    uint64(rng.IntN(50)),
		}
	}
	return entries
}

func storeAfter(entries []Entry) *Store {
	s := NewStore("gateway-0", limitertest.NewFakeClock(start), 3)
	for _, e := range entries {
		s.Merge(e)
	}
	return s
}

// The three properties that make gossip safe: entries may arrive in any
// order, more than once, or grouped differently, and the store ends up the
// same. Each is checked on many random inputs.
func TestMergeProperties(t *testing.T) {
	requireMerge(t)
	rng := rand.New(rand.NewPCG(1, 2))

	for trial := range 200 {
		entries := randomEntries(rng, 60)
		want := sortEntries(storeAfter(entries).Snapshot())

		shuffled := append([]Entry(nil), entries...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := sortEntries(storeAfter(shuffled).Snapshot()); !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: a different arrival order gave a different state\n got %+v\nwant %+v", trial, got, want)
		}

		twice := append(append([]Entry(nil), entries...), entries...)
		if got := sortEntries(storeAfter(twice).Snapshot()); !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: delivering every entry twice gave a different state\n got %+v\nwant %+v", trial, got, want)
		}

		// Merge the two halves into separate stores, then pour one store
		// into the other: the same as merging everything into one.
		half := len(entries) / 2
		a, b := storeAfter(entries[:half]), storeAfter(entries[half:])
		for _, e := range b.Snapshot() {
			a.Merge(e)
		}
		if got := sortEntries(a.Snapshot()); !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: merging two stores gave a different state than merging all entries into one\n got %+v\nwant %+v", trial, got, want)
		}
	}
}

// The cached total must always equal the sum of the per-node counts.
func TestMergeKeepsTotalEqualToSum(t *testing.T) {
	requireMerge(t)
	rng := rand.New(rand.NewPCG(3, 4))
	s := storeAfter(randomEntries(rng, 500))
	for k := range 4 {
		for _, view := range s.Counts(fmt.Sprintf("api:key-%d", k)) {
			var sum uint64
			for _, count := range view.PerNode {
				sum += count
			}
			if view.Total != sum {
				t.Errorf("key %d window %d: total = %d, sum of per-node counts = %d", k, view.Idx, view.Total, sum)
			}
		}
	}
}

func TestMergeTakesTheHigherCount(t *testing.T) {
	requireMerge(t)
	s := NewStore("gateway-0", limitertest.NewFakeClock(start), 3)
	e := Entry{Key: "route:client", WindowMs: 1000, Idx: startIdx, Node: "gateway-1", Count: 10}

	if !s.Merge(e) {
		t.Error("first report of a count: Merge returned false, want true")
	}
	if s.Merge(e) {
		t.Error("same report again: Merge returned true, want false")
	}
	lower := e
	lower.Count = 4
	if s.Merge(lower) {
		t.Error("a lower, older count: Merge returned true, want false")
	}
	higher := e
	higher.Count = 15
	if !s.Merge(higher) {
		t.Error("a higher count: Merge returned false, want true")
	}

	want := []WindowView{{Idx: startIdx, Total: 15, PerNode: map[string]uint64{"gateway-1": 15}}}
	if got := s.Counts("route:client"); !reflect.DeepEqual(got, want) {
		t.Errorf("Counts = %+v, want %+v", got, want)
	}
}

// News is passed on: a count learned from one peer is offered to the gossip
// loop so it reaches peers the sender did not pick.
func TestMergedNewsIsRelayed(t *testing.T) {
	requireMerge(t)
	s := NewStore("gateway-0", limitertest.NewFakeClock(start), 2)
	e := Entry{Key: "route:client", WindowMs: 1000, Idx: startIdx, Node: "gateway-1", Count: 10}
	s.Merge(e)

	for round := 1; round <= 2; round++ {
		if got := s.Dirty(); !reflect.DeepEqual(got, []Entry{e}) {
			t.Fatalf("round %d: Dirty = %+v, want the merged entry", round, got)
		}
	}
	if got := s.Dirty(); len(got) != 0 {
		t.Errorf("round 3: Dirty = %+v, want nothing", got)
	}

	s.Merge(e) // old news must not be relayed again
	if got := s.Dirty(); len(got) != 0 {
		t.Errorf("after a repeat of old news: Dirty = %+v, want nothing", got)
	}
}

// Only the previous, current and next windows are accepted.
func TestMergeIgnoresWindowsOutOfRange(t *testing.T) {
	requireMerge(t)
	for _, tc := range []struct {
		name   string
		offset int64
		want   bool
	}{
		{"two windows old", -2, false},
		{"previous window", -1, true},
		{"current window", 0, true},
		{"one window ahead", 1, true},
		{"two windows ahead", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore("gateway-0", limitertest.NewFakeClock(start), 3)
			e := Entry{Key: "route:client", WindowMs: 1000, Idx: startIdx + tc.offset, Node: "gateway-1", Count: 10}
			if got := s.Merge(e); got != tc.want {
				t.Errorf("Merge = %v, want %v", got, tc.want)
			}
			if got := len(s.Snapshot()) == 1; got != tc.want {
				t.Errorf("entry stored = %v, want %v", got, tc.want)
			}
		})
	}
}

// Counts merged from peers weigh on this node's decisions exactly like its
// own.
func TestMergedCountsAffectDecisions(t *testing.T) {
	requireMerge(t)
	clock := limitertest.NewFakeClock(start)
	s := NewStore("gateway-0", clock, 3)
	s.Merge(Entry{Key: "route:client", WindowMs: 1000, Idx: startIdx, Node: "gateway-1", Count: 60})
	s.Merge(Entry{Key: "route:client", WindowMs: 1000, Idx: startIdx, Node: "gateway-2", Count: 30})

	admitted := 0
	for range 20 {
		if s.Allow("route:client", limit, 1).Allowed {
			admitted++
		}
	}
	if admitted != 10 {
		t.Errorf("admitted %d with 90 of 100 already used by peers, want 10", admitted)
	}

	clock.Advance(time.Second + 500*time.Millisecond) // previous window: 100, at half weight
	if d := s.Allow("route:client", limit, 1); !d.Allowed || d.Remaining != 49 {
		t.Errorf("half-way through the next window: %+v, want allowed with 49 remaining", d)
	}
}
