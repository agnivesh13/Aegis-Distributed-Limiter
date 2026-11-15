package gossip

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// ErrInvalidPeriod is returned for a Period the wire format cannot carry.
// Entries state their window length in whole milliseconds.
var ErrInvalidPeriod = errors.New("limiter/gossip: period must be a whole number of milliseconds, at least 1")

// Config holds the gossip settings from the plan's configuration section.
type Config struct {
	Interval       time.Duration // time between gossip rounds
	Fanout         int           // peers each round's packets are sent to
	Retransmit     int           // rounds a changed count keeps being sent
	MaxPacketBytes int           // upper size of one gossip packet

	// OnDelivery, if set, is called with each received gossip packet's
	// age: this node's clock minus the packet's send time. It feeds the
	// gossip_delivery_seconds histogram.
	OnDelivery func(age time.Duration)
}

// DefaultConfig is the plan's defaults.
func DefaultConfig() Config {
	return Config{Interval: 100 * time.Millisecond, Fanout: 2, Retransmit: 3, MaxPacketBytes: 1200}
}

// Stats are running totals for the gossip metrics.
type Stats struct {
	PacketsSent    uint64
	BytesSent      uint64
	PacketsDropped uint64 // received packets that could not be decoded
	EntriesMerged  uint64 // received counts that told this node something new
}

// Node is the gossip limiter backend. It answers Allow from its Store and
// runs the loop that sends the store's changes to other nodes.
//
// Its NodeMeta, NotifyMsg, GetBroadcasts, LocalState and MergeRemoteState
// methods are exactly the methods of memberlist.Delegate, so a Node can be
// handed to memberlist as its delegate.
type Node struct {
	store *Store
	clock limiter.Clock
	cfg   Config

	mu        sync.Mutex // guards the fields below
	started   bool
	closed    bool
	transport Transport
	rng       *rand.Rand
	blocked   map[string]bool

	packetsSent    atomic.Uint64
	bytesSent      atomic.Uint64
	packetsDropped atomic.Uint64
	entriesMerged  atomic.Uint64

	stop chan struct{} // closed by Close to stop the loop
	done chan struct{} // closed by the loop when it has stopped
}

// NewNode returns a Node called self. It can answer Allow at once; it
// starts gossiping when Start is called.
func NewNode(self string, clock limiter.Clock, cfg Config) *Node {
	return &Node{
		store:   NewStore(self, clock, cfg.Retransmit),
		clock:   clock,
		cfg:     cfg,
		rng:     rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		blocked: make(map[string]bool),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start attaches the transport and begins the gossip loop. The transport
// is built after the Node because memberlist needs the Node as its delegate
// before it can join. Only the first call does anything.
func (n *Node) Start(t Transport) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started || n.closed {
		return
	}
	n.started = true
	n.transport = t
	go n.loop()
}

// Allow implements limiter.Limiter. Only the sliding window is supported:
// token bucket state does not merge cleanly between nodes.
func (n *Node) Allow(_ context.Context, key string, l limiter.Limit, cost int) (limiter.Decision, error) {
	if l.Algorithm != limiter.SlidingWindow {
		return limiter.Decision{}, limiter.ErrUnsupported
	}
	if cost < 1 || cost > l.Capacity() {
		return limiter.Decision{}, limiter.ErrInvalidCost
	}
	if l.Period < time.Millisecond || l.Period%time.Millisecond != 0 {
		return limiter.Decision{}, ErrInvalidPeriod
	}
	return n.store.Allow(key, l, cost), nil
}

// Close stops the gossip loop and closes the transport, which leaves the
// cluster. It is safe to call more than once. Allow keeps working after
// Close, from local counts only.
func (n *Node) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	started, t := n.started, n.transport
	n.mu.Unlock()

	close(n.stop)
	if !started {
		return nil
	}
	<-n.done
	if c, ok := t.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Store gives access to the counts, for /debug/limiter and the tracked-keys
// gauge.
func (n *Node) Store() *Store { return n.store }

// Stats returns the running gossip totals.
func (n *Node) Stats() Stats {
	return Stats{
		PacketsSent:    n.packetsSent.Load(),
		BytesSent:      n.bytesSent.Load(),
		PacketsDropped: n.packetsDropped.Load(),
		EntriesMerged:  n.entriesMerged.Load(),
	}
}

// Members reports the cluster size as this node sees it, itself included.
func (n *Node) Members() int {
	n.mu.Lock()
	t := n.transport
	n.mu.Unlock()
	if t == nil {
		return 1
	}
	return len(t.Peers()) + 1
}

// Block cuts this node off from peer, for partition tests: nothing is sent
// to peer, and counts that peer admitted are ignored wherever they arrive
// from. Blocking in both directions on both sides makes a full partition.
func (n *Node) Block(peer string) {
	n.mu.Lock()
	n.blocked[peer] = true
	n.mu.Unlock()
}

// Unblock reverses Block. The next full sync repairs what was missed.
func (n *Node) Unblock(peer string) {
	n.mu.Lock()
	delete(n.blocked, peer)
	n.mu.Unlock()
}

// loop runs a gossip round every interval and a sweep about once a second.
func (n *Node) loop() {
	defer close(n.done)
	ticker := time.NewTicker(n.cfg.Interval)
	defer ticker.Stop()
	lastSweep := time.Now()
	for {
		select {
		case <-n.stop:
			return
		case <-ticker.C:
			n.gossipOnce()
			if time.Since(lastSweep) >= time.Second {
				n.store.Sweep()
				lastSweep = time.Now()
			}
		}
	}
}

// gossipOnce is one round: gather the counts that changed, pack them, and
// send every packet to fanout peers chosen at random.
func (n *Node) gossipOnce() {
	entries := n.store.Dirty()
	if len(entries) == 0 {
		return
	}
	n.mu.Lock()
	t := n.transport
	n.mu.Unlock()
	if t == nil {
		return
	}
	peers := n.pickPeers(t.Peers())
	if len(peers) == 0 {
		return
	}

	packets := EncodePackets(TypeGossip, n.clock.Now().UnixMilli(), entries, n.cfg.MaxPacketBytes)
	for _, peer := range peers {
		for _, p := range packets {
			if err := t.Send(peer, p); err != nil {
				continue // best effort: retransmit rounds and full sync cover losses
			}
			n.packetsSent.Add(1)
			n.bytesSent.Add(uint64(len(p)))
		}
	}
}

// pickPeers chooses up to fanout peers at random, skipping blocked ones.
func (n *Node) pickPeers(peers []string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	open := peers[:0:0]
	for _, p := range peers {
		if !n.blocked[p] {
			open = append(open, p)
		}
	}
	n.rng.Shuffle(len(open), func(i, j int) { open[i], open[j] = open[j], open[i] })
	if len(open) > n.cfg.Fanout {
		open = open[:n.cfg.Fanout]
	}
	return open
}

// receive decodes a packet and merges its entries.
func (n *Node) receive(b []byte) {
	p, err := Decode(b)
	if err != nil {
		n.packetsDropped.Add(1)
		return
	}
	if p.Type == TypeGossip && n.cfg.OnDelivery != nil {
		n.cfg.OnDelivery(time.Duration(n.clock.Now().UnixMilli()-p.SentUnixMs) * time.Millisecond)
	}

	n.mu.Lock()
	var blocked map[string]bool
	if len(n.blocked) > 0 {
		blocked = make(map[string]bool, len(n.blocked))
		for peer := range n.blocked {
			blocked[peer] = true
		}
	}
	n.mu.Unlock()

	for _, e := range p.Entries {
		if blocked[e.Node] {
			continue
		}
		if n.store.Merge(e) {
			n.entriesMerged.Add(1)
		}
	}
}

// The five methods below are memberlist.Delegate.

// NodeMeta is unused: nodes carry no metadata.
func (n *Node) NodeMeta(limit int) []byte { return nil }

// NotifyMsg receives one gossip packet sent by another node's Send.
// memberlist reuses the buffer after this returns; Decode copies what it
// keeps, so nothing here holds on to it.
func (n *Node) NotifyMsg(b []byte) { n.receive(b) }

// GetBroadcasts is unused. The Node runs its own loop instead, so the
// gossip interval and fanout stay under this code's control: they are the
// variables the experiments change.
func (n *Node) GetBroadcasts(overhead, limit int) [][]byte { return nil }

// LocalState returns every live count. memberlist sends it to a peer over
// TCP when this node joins and at every full-sync interval, and the peer
// sends its own back. This repairs whatever UDP lost.
func (n *Node) LocalState(join bool) []byte {
	packets := EncodePackets(TypeFullSync, n.clock.Now().UnixMilli(), n.store.Snapshot(), 0)
	if len(packets) == 0 {
		return nil
	}
	return packets[0]
}

// MergeRemoteState receives a peer's LocalState.
func (n *Node) MergeRemoteState(buf []byte, join bool) {
	if len(buf) == 0 {
		return
	}
	n.receive(buf)
}
