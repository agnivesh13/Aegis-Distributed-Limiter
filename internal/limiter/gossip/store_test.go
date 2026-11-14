package gossip

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/limitertest"
)

var (
	start = time.Unix(1_700_000_000, 0) // window number 1_700_000_000 for a 1 s window
	limit = limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 100, Period: time.Second}
)

const startIdx = int64(1_700_000_000)

func testConfig() Config {
	return Config{Interval: time.Hour, Fanout: 2, Retransmit: 3, MaxPacketBytes: 1200}
}

func sortEntries(entries []Entry) []Entry {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Idx != b.Idx {
			return a.Idx < b.Idx
		}
		return a.Node < b.Node
	})
	return entries
}

// A single gossip node, with nobody to talk to, is an ordinary sliding
// window limiter and must pass the same suite as the other backends. The
// token bucket is refused, as the backend matrix in the plan says.
func TestConformance(t *testing.T) {
	limitertest.Run(t, func(t *testing.T, clock limiter.Clock) limiter.Limiter {
		return NewNode("gateway-1", clock, testConfig())
	}, limiter.SlidingWindow)
}

func TestAllowRejectsPeriodTheWireCannotCarry(t *testing.T) {
	n := NewNode("gateway-1", limitertest.NewFakeClock(start), testConfig())
	defer n.Close()
	l := limit
	l.Period = 1500 * time.Microsecond
	if _, err := n.Allow(context.Background(), "route:client", l, 1); err != ErrInvalidPeriod {
		t.Errorf("err = %v, want ErrInvalidPeriod", err)
	}
}

// A changed count is offered to the gossip loop for `retransmit` rounds and
// then no more, until it changes again.
func TestDirtyOffersAChangeForRetransmitRounds(t *testing.T) {
	s := NewStore("gateway-1", limitertest.NewFakeClock(start), 3)
	s.Allow("route:client", limit, 2)
	s.Allow("route:client", limit, 5)

	want := []Entry{{Key: "route:client", WindowMs: 1000, Idx: startIdx, Node: "gateway-1", Count: 7}}
	for round := 1; round <= 3; round++ {
		if got := s.Dirty(); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: Dirty = %+v, want %+v", round, got, want)
		}
	}
	if got := s.Dirty(); len(got) != 0 {
		t.Fatalf("round 4: Dirty = %+v, want nothing left to send", got)
	}

	s.Allow("route:client", limit, 1)
	want[0].Count = 8
	if got := s.Dirty(); !reflect.DeepEqual(got, want) {
		t.Errorf("after another request: Dirty = %+v, want %+v", got, want)
	}
}

// Windows older than the previous one cannot affect a decision, so they are
// neither gossiped nor kept.
func TestOldWindowsAreForgotten(t *testing.T) {
	clock := limitertest.NewFakeClock(start)
	s := NewStore("gateway-1", clock, 3)
	s.Allow("route:idle", limit, 1)
	clock.Advance(time.Second)
	s.Allow("route:busy", limit, 1)

	if got := len(s.Snapshot()); got != 2 {
		t.Fatalf("one window later: Snapshot has %d entries, want 2", got)
	}

	clock.Advance(time.Second) // route:idle's window is now two behind
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].Key != "route:busy" {
		t.Fatalf("two windows later: Snapshot = %+v, want only route:busy", snap)
	}
	for _, e := range s.Dirty() {
		if e.Key == "route:idle" {
			t.Errorf("Dirty still offers %+v from a dead window", e)
		}
	}

	if got := s.Len(); got != 2 {
		t.Fatalf("before Sweep: Len = %d, want 2", got)
	}
	s.Sweep()
	if got := s.Len(); got != 1 {
		t.Errorf("after Sweep: Len = %d, want 1", got)
	}
	clock.Advance(time.Second)
	s.Sweep()
	if got := s.Len(); got != 0 {
		t.Errorf("after both keys expire: Len = %d, want 0", got)
	}
}

func TestCountsShowsPerNodeView(t *testing.T) {
	clock := limitertest.NewFakeClock(start)
	s := NewStore("gateway-1", clock, 3)
	if got := s.Counts("route:client"); got != nil {
		t.Fatalf("unknown key: Counts = %+v, want nil", got)
	}
	s.Allow("route:client", limit, 3)
	clock.Advance(time.Second)
	s.Allow("route:client", limit, 4)

	want := []WindowView{
		{Idx: startIdx, Total: 3, PerNode: map[string]uint64{"gateway-1": 3}},
		{Idx: startIdx + 1, Total: 4, PerNode: map[string]uint64{"gateway-1": 4}},
	}
	if got := s.Counts("route:client"); !reflect.DeepEqual(got, want) {
		t.Errorf("Counts = %+v, want %+v", got, want)
	}
}

// recorder is a Transport that keeps what it is asked to send.
type recorder struct {
	mu    sync.Mutex
	peers []string
	sent  map[string][][]byte
}

func (r *recorder) Peers() []string { return r.peers }

func (r *recorder) Send(peer string, packet []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent == nil {
		r.sent = make(map[string][][]byte)
	}
	r.sent[peer] = append(r.sent[peer], append([]byte(nil), packet...))
	return nil
}

// One round sends every changed count to exactly `fanout` peers, in packets
// no larger than the configured size.
func TestGossipRoundSendsToFanoutPeers(t *testing.T) {
	cfg := testConfig()
	n := NewNode("gateway-1", limitertest.NewFakeClock(start), cfg)
	rec := &recorder{peers: []string{"gateway-2", "gateway-3", "gateway-4", "gateway-5"}}
	n.transport = rec

	const keys = 300
	for i := range keys {
		n.store.Allow("api:key-"+string(rune('a'+i%26))+string(rune('a'+i/26)), limit, 1)
	}
	n.gossipOnce()

	if len(rec.sent) != cfg.Fanout {
		t.Fatalf("packets went to %d peers, want %d", len(rec.sent), cfg.Fanout)
	}
	var packetCount, byteCount int
	for peer, packets := range rec.sent {
		entries := 0
		for _, raw := range packets {
			if len(raw) > cfg.MaxPacketBytes {
				t.Errorf("packet to %s is %d bytes, want at most %d", peer, len(raw), cfg.MaxPacketBytes)
			}
			p, err := Decode(raw)
			if err != nil {
				t.Fatalf("packet to %s: %v", peer, err)
			}
			entries += len(p.Entries)
			packetCount++
			byteCount += len(raw)
		}
		if entries != keys {
			t.Errorf("%s received %d entries, want %d", peer, entries, keys)
		}
	}
	if got := n.Stats(); got.PacketsSent != uint64(packetCount) || got.BytesSent != uint64(byteCount) {
		t.Errorf("Stats = %+v, want %d packets and %d bytes", got, packetCount, byteCount)
	}
}

func TestBlockedPeerIsNeverSentTo(t *testing.T) {
	n := NewNode("gateway-1", limitertest.NewFakeClock(start), testConfig())
	rec := &recorder{peers: []string{"gateway-2", "gateway-3"}}
	n.transport = rec
	n.Block("gateway-2")

	for range 20 { // enough rounds that a random pick would have hit gateway-2
		n.store.Allow("route:client", limit, 1)
		n.gossipOnce()
	}
	if len(rec.sent["gateway-2"]) != 0 {
		t.Errorf("sent %d packets to a blocked peer", len(rec.sent["gateway-2"]))
	}
	if len(rec.sent["gateway-3"]) == 0 {
		t.Error("sent nothing to the peer that is not blocked")
	}
}

func TestUndecodablePacketIsCountedAndDropped(t *testing.T) {
	n := NewNode("gateway-1", limitertest.NewFakeClock(start), testConfig())
	n.NotifyMsg([]byte{99, 1, 2, 3})
	n.NotifyMsg(nil)
	if got := n.Stats().PacketsDropped; got != 2 {
		t.Errorf("PacketsDropped = %d, want 2", got)
	}
}

func TestDeliveryAgeIsReported(t *testing.T) {
	clock := limitertest.NewFakeClock(start)
	var ages []time.Duration
	cfg := testConfig()
	cfg.OnDelivery = func(age time.Duration) { ages = append(ages, age) }
	n := NewNode("gateway-1", clock, cfg)

	packet := EncodePackets(TypeGossip, clock.Now().UnixMilli(), sampleEntries(1), 0)[0]
	clock.Advance(30 * time.Millisecond)
	n.NotifyMsg(packet)

	if len(ages) != 1 || ages[0] != 30*time.Millisecond {
		t.Errorf("delivery ages = %v, want [30ms]", ages)
	}
}

func TestCloseBeforeAndAfterStart(t *testing.T) {
	unstarted := NewNode("gateway-1", limitertest.NewFakeClock(start), testConfig())
	if err := unstarted.Close(); err != nil {
		t.Errorf("Close on a node that never started: %v", err)
	}

	cfg := testConfig()
	cfg.Interval = time.Millisecond
	n := NewNode("gateway-2", limitertest.NewFakeClock(start), cfg)
	n.Start(&recorder{peers: []string{"gateway-1"}})
	if got := n.Members(); got != 2 {
		t.Errorf("Members = %d, want 2", got)
	}
	if err := n.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if d, err := n.Allow(context.Background(), "route:client", limit, 1); err != nil || !d.Allowed {
		t.Errorf("Allow after Close = %+v, %v; want allowed", d, err)
	}
}
