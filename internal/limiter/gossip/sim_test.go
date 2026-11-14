package gossip

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/limitertest"
)

// These tests run a whole cluster inside one process: several Nodes joined
// by a fake network, all on one fake clock. Nothing is real, so every run
// is identical and a failure can be replayed exactly.

// fakeNet delivers packets between Nodes in the same process. It can lose
// a share of them, like UDP on a bad day.
type fakeNet struct {
	mu    sync.Mutex
	nodes map[string]*Node
	names []string
	rng   *rand.Rand
	loss  float64 // share of packets to drop, 0 to 1
	lost  int
}

func newFakeNet(seed uint64) *fakeNet {
	return &fakeNet{nodes: make(map[string]*Node), rng: rand.New(rand.NewPCG(seed, seed))}
}

// fakeTransport is one node's connection to a fakeNet.
type fakeTransport struct {
	net  *fakeNet
	self string
}

func (ft *fakeTransport) Peers() []string {
	ft.net.mu.Lock()
	defer ft.net.mu.Unlock()
	var peers []string
	for _, name := range ft.net.names {
		if name != ft.self {
			peers = append(peers, name)
		}
	}
	return peers
}

func (ft *fakeTransport) Send(peer string, packet []byte) error {
	ft.net.mu.Lock()
	target := ft.net.nodes[peer]
	drop := ft.net.rng.Float64() < ft.net.loss
	if drop {
		ft.net.lost++
	}
	ft.net.mu.Unlock()

	if target == nil || drop {
		return nil // lost on the way; the sender never finds out
	}
	target.NotifyMsg(append([]byte(nil), packet...))
	return nil
}

// cluster is n Nodes on one fakeNet and one fake clock. The gossip loops are
// not started: tests call round and fullSync so every step is deliberate.
type cluster struct {
	t     *testing.T
	clock *limitertest.FakeClock
	net   *fakeNet
	cfg   Config
	nodes []*Node
}

func newCluster(t *testing.T, n, fanout int) *cluster {
	t.Helper()
	requireMerge(t)
	c := &cluster{
		t:     t,
		clock: limitertest.NewFakeClock(start),
		net:   newFakeNet(42),
		cfg:   Config{Interval: time.Hour, Fanout: fanout, Retransmit: 3, MaxPacketBytes: 1200},
	}
	for i := range n {
		c.nodes = append(c.nodes, c.join(fmt.Sprintf("gateway-%d", i)))
	}
	return c
}

// join adds a fresh Node under the given name, replacing any Node already
// there. Replacing is how a test restarts a node with its memory gone.
func (c *cluster) join(name string) *Node {
	node := NewNode(name, c.clock, c.cfg)
	node.rng = rand.New(rand.NewPCG(uint64(len(name)), uint64(len(c.net.nodes))))
	node.transport = &fakeTransport{net: c.net, self: name}

	c.net.mu.Lock()
	if _, exists := c.net.nodes[name]; !exists {
		c.net.names = append(c.net.names, name)
	}
	c.net.nodes[name] = node
	c.net.mu.Unlock()
	return node
}

// admit sends n requests to a node and returns how many it allowed.
func (c *cluster) admit(node *Node, key string, l limiter.Limit, n int) int {
	allowed := 0
	for range n {
		if node.store.Allow(key, l, 1).Allowed {
			allowed++
		}
	}
	return allowed
}

// round runs one gossip round on every node, in order.
func (c *cluster) round() {
	for _, node := range c.nodes {
		node.gossipOnce()
	}
}

// fullSync does what memberlist's push-pull does, between every pair: each
// hands the other its whole state. It runs over TCP, so nothing is lost.
func (c *cluster) fullSync() {
	for _, a := range c.nodes {
		for _, b := range c.nodes {
			if a != b {
				b.MergeRemoteState(a.LocalState(false), false)
			}
		}
	}
}

// totals returns each node's total for the current window of key.
func (c *cluster) totals(key string) []uint64 {
	out := make([]uint64, len(c.nodes))
	for i, node := range c.nodes {
		for _, view := range node.store.Counts(key) {
			if view.Idx == startIdx {
				out[i] = view.Total
			}
		}
	}
	return out
}

func (c *cluster) wantAllTotals(key string, want uint64) {
	c.t.Helper()
	for i, got := range c.totals(key) {
		if got != want {
			c.t.Errorf("%s sees a total of %d, want %d (all totals: %v)", c.nodes[i].store.self, got, want, c.totals(key))
			return
		}
	}
}

var wide = limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 1000, Period: time.Second}

// admitUneven has node i admit i+1 requests, so every node holds a
// different count: 1 + 2 + ... + n in all.
func (c *cluster) admitUneven(key string) uint64 {
	var sum uint64
	for i, node := range c.nodes {
		sum += uint64(c.admit(node, key, wide, i+1))
	}
	return sum
}

// With fanout 4 in a cluster of 5, every node sends to every other node, so
// one round is enough for all of them to agree.
func TestFanoutToEveryPeerAgreesInOneRound(t *testing.T) {
	c := newCluster(t, 5, 4)
	sum := c.admitUneven("route:client")
	c.round()
	c.wantAllTotals("route:client", sum)
}

// With fanout 1 each round reaches one random peer. News spreads by being
// relayed, and a full sync settles whatever is still missing.
func TestFanoutOneAgreesAfterRoundsAndFullSync(t *testing.T) {
	c := newCluster(t, 5, 1)
	sum := c.admitUneven("route:client")
	for range c.cfg.Retransmit {
		c.round()
	}
	c.fullSync()
	c.wantAllTotals("route:client", sum)
}

// Losing 30% of gossip packets cannot corrupt a count; at worst a node is
// behind until the next full sync.
func TestLossyNetworkAgreesAfterFullSync(t *testing.T) {
	c := newCluster(t, 5, 2)
	c.net.loss = 0.3
	sum := c.admitUneven("route:client")
	for range c.cfg.Retransmit {
		c.round()
	}
	if c.net.lost == 0 {
		t.Fatal("the fake network dropped nothing, so this run tested no loss")
	}
	for i, got := range c.totals("route:client") {
		if got > sum {
			t.Errorf("node %d counted %d, above the true total of %d: loss must never inflate a count", i, got, sum)
		}
	}
	c.fullSync()
	c.wantAllTotals("route:client", sum)
}

// A node that restarts with empty memory learns its own earlier count back
// from its peers, and carries on counting from there.
func TestRestartedNodeRelearnsItsOwnCount(t *testing.T) {
	c := newCluster(t, 3, 2)
	c.admit(c.nodes[0], "route:client", wide, 7)
	c.admit(c.nodes[1], "route:client", wide, 2)
	c.round()
	c.wantAllTotals("route:client", 9)

	c.nodes[0] = c.join("gateway-0") // same name, memory gone
	if got := c.totals("route:client")[0]; got != 0 {
		t.Fatalf("restarted node starts with a total of %d, want 0", got)
	}
	c.fullSync()

	view := c.nodes[0].store.Counts("route:client")
	if len(view) != 1 || view[0].PerNode["gateway-0"] != 7 {
		t.Fatalf("after full sync the restarted node's own count is %+v, want 7", view)
	}

	c.admit(c.nodes[0], "route:client", wide, 1)
	c.round()
	c.wantAllTotals("route:client", 10)
}

// The known worst case from the plan: a burst that reaches every node
// before any gossip does. Each node admits the full limit, so the cluster
// admits N times the limit. Once the counts spread, everyone rejects.
func TestBurstFromIdleAdmitsLimitOnEveryNode(t *testing.T) {
	c := newCluster(t, 3, 2)
	admitted := 0
	for _, node := range c.nodes {
		admitted += c.admit(node, "route:client", limit, 150)
	}
	if admitted != 300 {
		t.Errorf("admitted %d before any gossip, want 300 (the limit of 100 on each of 3 nodes)", admitted)
	}

	c.round()
	c.wantAllTotals("route:client", 300)
	for _, node := range c.nodes {
		if d := node.store.Allow("route:client", limit, 1); d.Allowed {
			t.Errorf("%s still admits after learning the cluster total is 300", node.store.self)
		}
	}
}

// A partition: two halves that cannot hear each other each enforce the
// limit alone. When it heals, a full sync brings them back together.
func TestPartitionSplitsAndHeals(t *testing.T) {
	c := newCluster(t, 4, 3)
	left, right := c.nodes[:2], c.nodes[2:]
	for _, a := range left {
		for _, b := range right {
			a.Block(b.store.self)
			b.Block(a.store.self)
		}
	}

	c.admit(left[0], "route:client", wide, 5)
	c.admit(right[0], "route:client", wide, 8)
	for range c.cfg.Retransmit {
		c.round()
	}
	c.fullSync()
	if got, want := c.totals("route:client"), []uint64{5, 5, 8, 8}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("during the partition totals are %v, want %v", got, want)
	}

	for _, a := range left {
		for _, b := range right {
			a.Unblock(b.store.self)
			b.Unblock(a.store.self)
		}
	}
	c.fullSync()
	c.wantAllTotals("route:client", 13)
}

// The real loop, on a real ticker: counts admitted on one node show up on
// another without the test driving anything.
func TestStartedNodesConvergeOnTheirOwn(t *testing.T) {
	requireMerge(t)
	clock := limitertest.NewFakeClock(start)
	net := newFakeNet(7)
	cfg := Config{Interval: 2 * time.Millisecond, Fanout: 2, Retransmit: 3, MaxPacketBytes: 1200}

	var nodes []*Node
	for i := range 3 {
		name := fmt.Sprintf("gateway-%d", i)
		node := NewNode(name, clock, cfg)
		net.mu.Lock()
		net.nodes[name] = node
		net.names = append(net.names, name)
		net.mu.Unlock()
		nodes = append(nodes, node)
	}
	for _, node := range nodes {
		node.Start(&fakeTransport{net: net, self: node.store.self})
		defer node.Close()
	}

	// Admit from several goroutines while the loops run, so the race
	// detector sees Allow, the gossip loop and NotifyMsg together.
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Go(func() {
			for range 20 {
				node.store.Allow("route:client", wide, 1)
			}
		})
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		agreed := true
		for _, node := range nodes {
			view := node.store.Counts("route:client")
			if len(view) != 1 || view[0].Total != 60 {
				agreed = false
			}
		}
		if agreed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nodes did not converge on a total of 60 within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}
