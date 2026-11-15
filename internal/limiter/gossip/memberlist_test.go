package gossip

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// inbox is a memberlist delegate that keeps the messages it receives.
type inbox struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (i *inbox) NotifyMsg(b []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.msgs = append(i.msgs, append([]byte(nil), b...))
}
func (i *inbox) count() int                             { i.mu.Lock(); defer i.mu.Unlock(); return len(i.msgs) }
func (i *inbox) NodeMeta(int) []byte                    { return nil }
func (i *inbox) GetBroadcasts(int, int) [][]byte        { return nil }
func (i *inbox) LocalState(bool) []byte                 { return nil }
func (i *inbox) MergeRemoteState(buf []byte, join bool) {}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Three real memberlist instances on localhost: they find each other
// through one join address, see each other as peers, carry packets
// between them, and notice when one leaves.
func TestMemberlistTransport(t *testing.T) {
	var (
		transports []*Memberlist
		inboxes    []*inbox
	)
	for i := range 3 {
		in := &inbox{}
		// Port 0 asks for any free port. memberlist needs the same number
		// free for TCP and UDP, and on Windows some UDP ports are reserved
		// (for Hyper-V and Docker), so an attempt can fail: try again.
		var (
			m   *Memberlist
			err error
		)
		for range 10 {
			m, err = NewMemberlist(MemberlistConfig{
				Name: fmt.Sprintf("gateway-%d", i), BindAddr: "127.0.0.1", BindPort: 0,
			}, in)
			if err == nil {
				break
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Close() })
		transports = append(transports, m)
		inboxes = append(inboxes, in)
	}
	for _, m := range transports[1:] {
		if n, err := m.Join([]string{transports[0].Addr()}); err != nil || n != 1 {
			t.Fatalf("join: %d nodes, %v", n, err)
		}
	}

	eventually(t, "every node sees two peers", func() bool {
		for _, m := range transports {
			if len(m.Peers()) != 2 {
				return false
			}
		}
		return true
	})
	peers := transports[0].Peers()
	sort.Strings(peers)
	if fmt.Sprint(peers) != "[gateway-1 gateway-2]" {
		t.Errorf("gateway-0 sees %v", peers)
	}

	packet := []byte{wireVersion, TypeGossip, 1, 0}
	if err := transports[0].Send("gateway-2", packet); err != nil {
		t.Fatal(err)
	}
	eventually(t, "gateway-2 receives the packet", func() bool { return inboxes[2].count() == 1 })
	if got := inboxes[2].msgs[0]; !bytes.Equal(got, packet) {
		t.Errorf("received %v, want %v", got, packet)
	}
	if inboxes[1].count() != 0 {
		t.Error("a packet for gateway-2 reached gateway-1")
	}
	if err := transports[0].Send("nobody", packet); err == nil {
		t.Error("sending to an unknown member succeeded")
	}

	if err := transports[2].Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "gateway-0 drops the node that left", func() bool { return len(transports[0].Peers()) == 1 })
}
