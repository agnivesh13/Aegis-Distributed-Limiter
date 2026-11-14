package gossip

import (
	"encoding/binary"
	"errors"
	"math"
)

// The wire format, from the plan:
//
//	packet := version:u8  type:u8  sent_unix_ms:uvarint  n:uvarint  entry{n}
//	entry  := key:bytes  window_ms:uvarint  idx:uvarint  node:bytes  count:uvarint
//	bytes  := len:uvarint  data
//
// A uvarint stores a whole number in as few bytes as it needs: small numbers
// take one byte, large ones up to ten.

const wireVersion = 1

// Packet types.
const (
	TypeGossip   byte = 1 // a round's changed entries, sent over UDP
	TypeFullSync byte = 2 // every live entry, exchanged over TCP
)

// Entry says: for this key, in window number Idx of length WindowMs, node
// Node has admitted Count requests in total.
type Entry struct {
	Key      string
	WindowMs int64
	Idx      int64
	Node     string
	Count    uint64
}

// Packet is one decoded message.
type Packet struct {
	Type       byte
	SentUnixMs int64
	Entries    []Entry
}

var (
	ErrUnknownVersion = errors.New("gossip: unknown packet version")
	ErrMalformed      = errors.New("gossip: malformed packet")
)

// EncodePackets packs entries into as few packets as possible, none longer
// than maxBytes. A maxBytes of 0 means no limit: everything goes in one
// packet, which is what a full sync over TCP wants.
//
// One entry is never split. An entry too big to fit in maxBytes by itself
// (a very long key) is sent alone in a packet that exceeds the limit.
func EncodePackets(typ byte, sentUnixMs int64, entries []Entry, maxBytes int) [][]byte {
	if len(entries) == 0 {
		return nil
	}
	sent := binary.AppendUvarint(nil, uint64(sentUnixMs))
	// The fixed part of every packet: version, type, timestamp. The entry
	// count follows and its size depends on the count.
	fixed := 2 + len(sent)

	var (
		packets [][]byte
		body    []byte // the encoded entries of the packet being filled
		count   int
	)
	flush := func() {
		p := make([]byte, 0, fixed+binary.MaxVarintLen64+len(body))
		p = append(p, wireVersion, typ)
		p = append(p, sent...)
		p = binary.AppendUvarint(p, uint64(count))
		p = append(p, body...)
		packets = append(packets, p)
		body, count = body[:0], 0
	}

	var one []byte
	for _, e := range entries {
		one = appendEntry(one[:0], e)
		if maxBytes > 0 && count > 0 && fixed+uvarintLen(uint64(count+1))+len(body)+len(one) > maxBytes {
			flush()
		}
		body = append(body, one...)
		count++
	}
	flush()
	return packets
}

func appendEntry(b []byte, e Entry) []byte {
	b = binary.AppendUvarint(b, uint64(len(e.Key)))
	b = append(b, e.Key...)
	b = binary.AppendUvarint(b, uint64(e.WindowMs))
	b = binary.AppendUvarint(b, uint64(e.Idx))
	b = binary.AppendUvarint(b, uint64(len(e.Node)))
	b = append(b, e.Node...)
	b = binary.AppendUvarint(b, e.Count)
	return b
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// Decode parses one packet. It accepts any bytes without panicking: packets
// arrive over UDP from the network and may be cut short or corrupted.
// The entries it returns do not share memory with b.
func Decode(b []byte) (Packet, error) {
	if len(b) < 2 {
		return Packet{}, ErrMalformed
	}
	if b[0] != wireVersion {
		return Packet{}, ErrUnknownVersion
	}
	p := Packet{Type: b[1]}
	if p.Type != TypeGossip && p.Type != TypeFullSync {
		return Packet{}, ErrMalformed
	}
	r := reader{buf: b[2:]}

	p.SentUnixMs = r.int64()
	n := r.uvarint()
	// Every entry takes at least 5 bytes, so a count larger than that
	// allows is a lie. Checking first stops a tiny packet that claims a
	// billion entries from making us reserve memory for them.
	const minEntryBytes = 5
	if r.failed || n > uint64(len(r.buf)/minEntryBytes) {
		return Packet{}, ErrMalformed
	}

	p.Entries = make([]Entry, 0, n)
	for range n {
		e := Entry{
			Key:      r.string(),
			WindowMs: r.int64(),
			Idx:      r.int64(),
			Node:     r.string(),
			Count:    r.uvarint(),
		}
		if r.failed {
			return Packet{}, ErrMalformed
		}
		p.Entries = append(p.Entries, e)
	}
	if len(r.buf) != 0 { // bytes left over after the last entry
		return Packet{}, ErrMalformed
	}
	return p, nil
}

// reader walks a buffer. After the first bad read it sets failed and
// returns zero values, so callers check once at the end and not after
// every field.
type reader struct {
	buf    []byte
	failed bool
}

func (r *reader) uvarint() uint64 {
	if r.failed {
		return 0
	}
	v, n := binary.Uvarint(r.buf)
	if n <= 0 {
		r.failed = true
		return 0
	}
	r.buf = r.buf[n:]
	return v
}

// int64 reads a uvarint that must fit in a signed 64-bit number.
func (r *reader) int64() int64 {
	v := r.uvarint()
	if v > math.MaxInt64 {
		r.failed = true
		return 0
	}
	return int64(v)
}

func (r *reader) string() string {
	n := r.uvarint()
	if r.failed || n > uint64(len(r.buf)) {
		r.failed = true
		return ""
	}
	s := string(r.buf[:n]) // string() copies, so nothing keeps the packet buffer alive
	r.buf = r.buf[n:]
	return s
}
