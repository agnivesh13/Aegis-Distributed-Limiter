package gossip

// Transport is how a Node reaches the other nodes. The real implementation
// wraps hashicorp/memberlist; tests use an in-process fake.
//
// A Transport that also has a Close method (io.Closer) is closed when the
// Node closes, which is where the real one leaves the cluster.
type Transport interface {
	// Peers returns the IDs of the other live nodes. It never includes
	// this node.
	Peers() []string

	// Send delivers one packet to peer on a best-effort basis: it may be
	// lost, duplicated or arrive out of order.
	Send(peer string, packet []byte) error
}
