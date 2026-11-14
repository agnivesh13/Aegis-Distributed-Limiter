package gossip

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func sampleEntries(n int) []Entry {
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{
			Key:      fmt.Sprintf("api:key-%d", i),
			WindowMs: 1000,
			Idx:      1_700_000_000 + int64(i%2),
			Node:     fmt.Sprintf("gateway-%d", i%5),
			Count:    uint64(i * 37),
		}
	}
	return entries
}

func decodeAll(t *testing.T, packets [][]byte) []Entry {
	t.Helper()
	var all []Entry
	for _, raw := range packets {
		p, err := Decode(raw)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		all = append(all, p.Entries...)
	}
	return all
}

func TestRoundTrip(t *testing.T) {
	entries := []Entry{
		{Key: "search:key-123", WindowMs: 1000, Idx: 1_700_000_000, Node: "gateway-1", Count: 42},
		{Key: "", WindowMs: 1, Idx: 0, Node: "", Count: 0},
		{Key: "api:10.0.0.7", WindowMs: math.MaxInt64, Idx: math.MaxInt64, Node: "g", Count: math.MaxUint64},
	}
	packets := EncodePackets(TypeFullSync, 1_700_000_000_123, entries, 0)
	if len(packets) != 1 {
		t.Fatalf("got %d packets with no size limit, want 1", len(packets))
	}
	p, err := Decode(packets[0])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if p.Type != TypeFullSync || p.SentUnixMs != 1_700_000_000_123 {
		t.Errorf("header = type %d, sent %d; want type %d, sent 1700000000123", p.Type, p.SentUnixMs, TypeFullSync)
	}
	if !reflect.DeepEqual(p.Entries, entries) {
		t.Errorf("entries = %+v, want %+v", p.Entries, entries)
	}
}

func TestNoEntriesMeansNoPackets(t *testing.T) {
	if packets := EncodePackets(TypeGossip, 1, nil, 1200); packets != nil {
		t.Errorf("got %d packets for no entries, want none", len(packets))
	}
}

// Entries are split over several packets, each within the size limit, and
// nothing is lost or reordered on the way.
func TestSplitsIntoPacketsWithinTheLimit(t *testing.T) {
	entries := sampleEntries(500)
	for _, maxBytes := range []int{64, 200, 1200} {
		t.Run(fmt.Sprint(maxBytes), func(t *testing.T) {
			packets := EncodePackets(TypeGossip, 1_700_000_000_000, entries, maxBytes)
			if len(packets) < 2 {
				t.Fatalf("got %d packets, want the entries split over several", len(packets))
			}
			for i, p := range packets {
				if len(p) > maxBytes {
					t.Errorf("packet %d is %d bytes, want at most %d", i, len(p), maxBytes)
				}
			}
			if got := decodeAll(t, packets); !reflect.DeepEqual(got, entries) {
				t.Errorf("decoded %d entries that differ from the %d encoded", len(got), len(entries))
			}
		})
	}
}

// An entry that cannot fit in the limit at all still goes out, alone.
func TestOversizedEntryIsSentAlone(t *testing.T) {
	big := Entry{Key: string(bytes.Repeat([]byte("k"), 300)), WindowMs: 1000, Idx: 5, Node: "n", Count: 1}
	small := Entry{Key: "a", WindowMs: 1000, Idx: 5, Node: "n", Count: 2}
	packets := EncodePackets(TypeGossip, 1, []Entry{small, big, small}, 100)
	if len(packets) != 3 {
		t.Fatalf("got %d packets, want 3 (small, big alone, small)", len(packets))
	}
	if got := decodeAll(t, packets); !reflect.DeepEqual(got, []Entry{small, big, small}) {
		t.Errorf("entries changed in transit: %+v", got)
	}
}

func TestDecodeRejectsBadPackets(t *testing.T) {
	good := EncodePackets(TypeGossip, 1_700_000_000_000, sampleEntries(3), 0)[0]

	unknownVersion := bytes.Clone(good)
	unknownVersion[0] = 2
	if _, err := Decode(unknownVersion); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("version 2: err = %v, want ErrUnknownVersion", err)
	}

	unknownType := bytes.Clone(good)
	unknownType[1] = 9
	if _, err := Decode(unknownType); !errors.Is(err, ErrMalformed) {
		t.Errorf("type 9: err = %v, want ErrMalformed", err)
	}

	// A packet cut short at any point must be refused, never half-read.
	for cut := range len(good) {
		if _, err := Decode(good[:cut]); err == nil {
			t.Errorf("packet cut to %d of %d bytes was accepted", cut, len(good))
		}
	}

	if _, err := Decode(append(bytes.Clone(good), 0)); !errors.Is(err, ErrMalformed) {
		t.Errorf("trailing byte: err = %v, want ErrMalformed", err)
	}

	// Two bytes of header, a timestamp, then a claim of 2^62 entries.
	liar := []byte{wireVersion, TypeGossip, 1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x3f}
	if _, err := Decode(liar); !errors.Is(err, ErrMalformed) {
		t.Errorf("huge entry count: err = %v, want ErrMalformed", err)
	}
}

// FuzzDecode feeds Decode arbitrary bytes. It must never panic, and whatever
// it accepts must survive being encoded and decoded again unchanged.
//
// Run it for longer with: go test -fuzz FuzzDecode ./internal/limiter/gossip/
func FuzzDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{wireVersion, TypeGossip})
	f.Add(EncodePackets(TypeGossip, 1_700_000_000_000, sampleEntries(4), 0)[0])
	f.Add(EncodePackets(TypeFullSync, 0, []Entry{{}}, 0)[0])

	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Decode(b)
		if err != nil {
			return
		}
		if len(p.Entries) == 0 {
			return // EncodePackets sends nothing for no entries
		}
		again, err := Decode(EncodePackets(p.Type, p.SentUnixMs, p.Entries, 0)[0])
		if err != nil {
			t.Fatalf("re-encoded packet failed to decode: %v", err)
		}
		if !reflect.DeepEqual(again, p) {
			t.Fatalf("round trip changed the packet:\n got %+v\nwant %+v", again, p)
		}
	})
}
