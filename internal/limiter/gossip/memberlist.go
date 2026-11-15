package gossip

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

// The Node's five delegate methods are exactly memberlist.Delegate. This
// line makes the compiler check it.
var _ memberlist.Delegate = (*Node)(nil)

// MemberlistConfig is what the real transport needs to know.
type MemberlistConfig struct {
	Name             string        // this node's ID, unique in the cluster
	BindAddr         string        // address to listen on; empty means all
	BindPort         int           // gossip port, TCP and UDP; 0 picks a free one
	FullSyncInterval time.Duration // how often to exchange full state with one peer
	LogOutput        io.Writer     // memberlist's own log lines; nil discards them
}

// Memberlist is the Transport that runs over hashicorp/memberlist: it
// finds the other gateways, notices when they die, carries the gossip
// packets over UDP and the full syncs over TCP.
type Memberlist struct {
	ml *memberlist.Memberlist

	closeOnce sync.Once
	closeErr  error
}

// NewMemberlist starts memberlist with delegate receiving the messages. In
// the gateway the delegate is the Node; tests may pass anything.
func NewMemberlist(cfg MemberlistConfig, delegate memberlist.Delegate) (*Memberlist, error) {
	c := memberlist.DefaultLANConfig()
	c.Name = cfg.Name
	if cfg.BindAddr != "" {
		c.BindAddr = cfg.BindAddr
		c.AdvertiseAddr = cfg.BindAddr
	}
	c.BindPort = cfg.BindPort
	c.AdvertisePort = cfg.BindPort
	c.Delegate = delegate
	if cfg.FullSyncInterval > 0 {
		c.PushPullInterval = cfg.FullSyncInterval
	}
	out := cfg.LogOutput
	if out == nil {
		out = io.Discard
	}
	c.Logger = log.New(out, "memberlist: ", log.LstdFlags)

	ml, err := memberlist.Create(c)
	if err != nil {
		return nil, fmt.Errorf("gossip: starting memberlist: %w", err)
	}
	return &Memberlist{ml: ml}, nil
}

// Join contacts the given host:port addresses. A name that resolves to
// several addresses (the Compose service name) reaches all of them. It
// returns how many nodes answered.
func (m *Memberlist) Join(addrs []string) (int, error) {
	return m.ml.Join(addrs)
}

// Port is the port memberlist is listening on, useful when it was 0.
func (m *Memberlist) Port() int {
	return int(m.ml.LocalNode().Port)
}

// Addr is this node's host:port as other nodes should dial it.
func (m *Memberlist) Addr() string {
	return m.ml.LocalNode().Address()
}

// Peers returns the names of the live members other than this node.
func (m *Memberlist) Peers() []string {
	self := m.ml.LocalNode().Name
	var peers []string
	for _, n := range m.ml.Members() {
		if n.Name != self {
			peers = append(peers, n.Name)
		}
	}
	return peers
}

// Send delivers one packet to peer over UDP, without acknowledgement.
func (m *Memberlist) Send(peer string, packet []byte) error {
	for _, n := range m.ml.Members() {
		if n.Name == peer {
			return m.ml.SendBestEffort(n, packet)
		}
	}
	return fmt.Errorf("gossip: no live member called %q", peer)
}

// Close tells the others this node is leaving, then stops memberlist. It
// is safe to call more than once: memberlist itself panics on a second
// Leave.
func (m *Memberlist) Close() error {
	m.closeOnce.Do(func() {
		leaveErr := m.ml.Leave(time.Second)
		if err := m.ml.Shutdown(); err != nil {
			m.closeErr = err
			return
		}
		m.closeErr = leaveErr
	})
	return m.closeErr
}
