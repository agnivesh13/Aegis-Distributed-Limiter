# 09. The gossip backend: sharing a limit without Redis

Files: [store.go](../internal/limiter/gossip/store.go), [merge.go](../internal/limiter/gossip/merge.go), [codec.go](../internal/limiter/gossip/codec.go), [node.go](../internal/limiter/gossip/node.go), [transport.go](../internal/limiter/gossip/transport.go)

## The idea

The Redis backend is exact, but every request pays for a network round trip to Redis, and if Redis is down the limiter cannot decide.

The gossip backend makes the opposite trade:

- **Every request is decided from the gateway's own memory.** No network call at all, so it is as fast as the memory backend.
- **In the background, gateways tell each other what they admitted.** A few times a second, each one sends its recent counts to a couple of others, who pass them on. News spreads the way gossip does in a village, which is where the name comes from.

The price: a gateway's picture of the others is always slightly out of date. Requests admitted elsewhere in the last moment are invisible to it, so the cluster can let through a little more than the limit. How much more is what Phase 4 measures.

## The hard question: how do you merge counts?

Suppose gateways simply sent each other "I just admitted 5 more". Networks lose packets, deliver them twice, and deliver them out of order:

- A packet delivered **twice** adds 5 twice. The count is now wrong for ever.
- A packet that is **lost** leaves the count short for ever.

The fix is to change *what is sent*. A gateway never says "5 more". It says its **running total**: "in window 1700000000, I, gateway-1, have admitted 42 so far".

Each gateway keeps, for each window, a small table with one row per gateway:

```text
key "search:key-123", window 1700000000

  gateway-1   42
  gateway-2   17
  gateway-3   30
  ---------------
  total       89
```

When a report arrives, the rule is: **keep the larger number**.

- "gateway-2 has admitted 20": 20 is more than 17, so the row becomes 20 and the total rises by 3.
- "gateway-2 has admitted 20" again: 20 is not more than 20. Nothing happens. A duplicate is harmless.
- "gateway-2 has admitted 15", an old packet arriving late: 15 is less than 20. Nothing happens. Reordering is harmless.
- A lost packet: the next report carries a higher running total anyway, so nothing is lost for good.

A gateway's own count only ever goes up, which is why "larger" always means "newer". This structure is called a **grow-only counter**, or **G-Counter**. It belongs to a family called **CRDTs** (conflict-free replicated data types): data built so that copies on different machines can be merged in any order and always end up equal.

Taking the maximum has three properties, and the tests check each:

| Property | Meaning | Why the network needs it |
| --- | --- | --- |
| Commutative | Order does not matter | Packets arrive out of order |
| Associative | Grouping does not matter | News arrives by different routes |
| Idempotent | Applying it twice changes nothing | Packets get duplicated |

Writing this merge is your piece ([merge.go](../internal/limiter/gossip/merge.go)). The stub there lists what to build and three questions to think through first.

## The store

```go
type windowCounts struct {
	perNode map[string]uint64 // node ID -> requests that node admitted in this window
	total   uint64            // cached sum of perNode
	dirty   map[string]int    // node ID -> gossip rounds this count still has to be sent
}
```

- `perNode` is the table above.
- `total` is the sum, kept up to date so a decision does not have to add the rows up every time.
- `dirty` is the list of rows that changed and still need telling to others.

### Deciding a request

```go
d := limiter.SlidingWindowDecision(l, cost, cur, prev, elapsed)
if d.Allowed {
	wc := ks.window(idx)
	wc.perNode[s.self] += uint64(cost)
	wc.total += uint64(cost)
	wc.dirty[s.self] = s.retransmit
}
```

The decision is the same sliding window arithmetic as the memory backend: literally the same function, shared through [window.go](../internal/limiter/window.go). The only difference is where `cur` and `prev` come from. Here they are the *totals across all nodes* for the current and previous window.

An allowed request raises this node's own row and marks it dirty.

### What "dirty" and "retransmit" mean

`dirty[node] = 3` means "send this row in the next 3 gossip rounds". Each round uses one up:

```go
if rounds <= 1 {
	delete(wc.dirty, node)
} else {
	wc.dirty[node] = rounds - 1
}
```

Why more than once? Gossip goes over UDP, which may lose a packet without telling anyone. Sending a change in three rounds, each to different random peers, makes it very likely to arrive somewhere.

And when a node *learns* something new by merging, it marks that row dirty too. That is the relay: news a node hears from one peer, it passes on to others.

## The wire format

A packet has to be turned into bytes to cross the network. The plan fixes the layout:

```text
packet := version:u8  type:u8  sent_unix_ms:uvarint  n:uvarint  entry{n}
entry  := key:bytes  window_ms:uvarint  idx:uvarint  node:bytes  count:uvarint
bytes  := len:uvarint  data
```

Read it as: one byte for the version, one byte for the type, the send time, how many entries follow, then the entries.

**What is a uvarint?** A way to store a whole number in as few bytes as it needs. A fixed 8-byte number wastes space when the value is 5. A uvarint uses 7 bits of each byte for the number and the 8th bit to say "more bytes follow". Small numbers take one byte; large ones take more. Go's `encoding/binary` package does the work:

```go
b = binary.AppendUvarint(b, e.Count)   // write
v, n := binary.Uvarint(r.buf)          // read: value, and bytes used
```

**Why a version byte?** If the format ever changes, old and new gateways will run side by side during an upgrade. The first byte lets a node recognise a packet it cannot read and drop it cleanly.

### Splitting into packets

UDP packets above roughly 1,400 bytes get broken up by the network, and then losing any piece loses the whole packet. So `EncodePackets` fills a packet up to `max_packet_bytes` (1,200), then starts another. An entry is never cut in half.

### Decoding bytes you cannot trust

Anything can arrive from a network: a packet cut short, random noise, or something crafted to cause harm. `Decode` must never crash, whatever it is given.

Two defences worth copying:

```go
func (r *reader) string() string {
	n := r.uvarint()
	if r.failed || n > uint64(len(r.buf)) {
		r.failed = true
		return ""
	}
```

Before reading `n` bytes, check that `n` bytes are actually there. Without that check, a packet claiming a 4 GB key would crash the program.

```go
const minEntryBytes = 5
if r.failed || n > uint64(len(r.buf)/minEntryBytes) {
	return Packet{}, ErrMalformed
}
p.Entries = make([]Entry, 0, n)
```

The packet states how many entries it holds, and the code reserves memory for that many. A ten-byte packet claiming a billion entries would make the gateway reserve gigabytes. Every entry needs at least 5 bytes, so a claim larger than `remaining bytes / 5` is a lie and is refused *before* any memory is reserved.

The `reader` type also shows a tidy error-handling pattern: after the first bad read it sets `failed` and returns zeros from then on. The calling code reads all five fields and checks `failed` once, with no `if err != nil` after every field.

## Fuzzing

How do you test "never crashes on any input"? You cannot list every input. **Fuzzing** has the computer invent them:

```go
func FuzzDecode(f *testing.F) {
	f.Add(...)   // a few starting examples
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Decode(b)
		...
	})
}
```

Go mutates the starting examples in millions of ways, flipping bits, cutting, repeating, and watches for a crash. It also watches which lines of code each input reaches, and keeps inputs that reach new ones, so it steadily digs into the corners.

The fuzz test checks a second thing: anything `Decode` accepts must come out the same after being encoded and decoded again.

```
go test -fuzz FuzzDecode -fuzztime 20s ./internal/limiter/gossip/
```

I ran it for 20 seconds: about 2.5 million inputs, no crash, no mismatch.

## The node: the loop and the hooks

`Store` is pure data and arithmetic. `Node` wraps it with everything that involves time and the network.

### One gossip round

```go
func (n *Node) gossipOnce() {
	entries := n.store.Dirty()                 // what changed
	...
	peers := n.pickPeers(t.Peers())            // choose `fanout` peers at random
	packets := EncodePackets(TypeGossip, ...)  // pack it
	for _, peer := range peers {
		for _, p := range packets {
			t.Send(peer, p)
```

A background goroutine calls this every `interval` (100 ms by default). It has the same ticker-and-stop-channel shape as the janitor in [05-memory-backend.md](05-memory-backend.md).

**`fanout`** is how many peers each round goes to. With fanout 2 in a cluster of 5, a round reaches two nodes directly and the rest by relay. Interval and fanout are the two dials the experiments turn: a shorter interval and a larger fanout mean fresher counts and more network traffic.

### Membership: who is out there?

To gossip, a node must know which other nodes exist and which have died. That problem is handed to a library, `hashicorp/memberlist`. The library calls back into our code through five methods, its **delegate** interface:

| Method | When memberlist calls it | What the node does |
| --- | --- | --- |
| `NotifyMsg(b)` | A packet from a peer arrived | Decode it and merge each entry |
| `LocalState()` | A full sync is starting | Return every live count |
| `MergeRemoteState(b)` | A peer's full state arrived | Decode it and merge each entry |
| `GetBroadcasts()` | It has room to send our messages | Nothing: we run our own loop |
| `NodeMeta()` | It wants extra data about the node | Nothing |

`Node` has exactly these five methods, so it can be handed to memberlist as its delegate. Notice `node.go` does not even import memberlist: the methods use only basic types. Go's interfaces need no declaration, as in [02-go-basics.md](02-go-basics.md).

### Full sync: the safety net

Gossip packets can be lost, and after three rounds a change is no longer resent. So every few seconds memberlist picks a peer and the two exchange their *whole* state over TCP, which does not lose data. Whatever gossip missed, this repairs. The plan calls it full sync; the general name is **anti-entropy**.

### The `Transport` interface

```go
type Transport interface {
	Peers() []string
	Send(peer string, packet []byte) error
}
```

The node does not talk to memberlist directly. It talks to this small interface. The real implementation will wrap memberlist (an Opus task). The tests use a fake that delivers packets inside one process. It is the same idea as the injected clock: put the awkward, real-world thing behind an interface, and tests can replace it.

## Simulating a cluster in one process

Testing a distributed system with real machines is slow, and failures are hard to repeat, because timing differs on every run. So `sim_test.go` builds a whole cluster in one process:

- several `Node`s,
- a `fakeNet` that carries packets between them, and can drop a share of them,
- one `FakeClock` shared by all.

The gossip loops are *not* started. The test calls `round()` and `fullSync()` itself, so each step happens exactly when the test says. With seeded random numbers, every run is identical. This is called **deterministic simulation**.

| Test | What it shows |
| --- | --- |
| `TestFanoutToEveryPeerAgreesInOneRound` | 5 nodes, fanout 4: one round and everyone agrees |
| `TestFanoutOneAgreesAfterRoundsAndFullSync` | With fanout 1, relaying plus a full sync gets everyone there |
| `TestLossyNetworkAgreesAfterFullSync` | 30% of packets dropped: no count is ever inflated, and full sync repairs the gaps |
| `TestRestartedNodeRelearnsItsOwnCount` | A node that lost its memory gets its own earlier count back from its peers |
| `TestBurstFromIdleAdmitsLimitOnEveryNode` | The known worst case: a burst hits all 3 nodes before any gossip, and 300 are admitted against a limit of 100 |
| `TestPartitionSplitsAndHeals` | Two halves that cannot hear each other each count alone; after healing, a full sync reunites them |
| `TestStartedNodesConvergeOnTheirOwn` | The real loop on a real ticker, with requests arriving from several goroutines at once |

The burst test is not a bug report. It writes down a limit of the design, stated in the plan, as a test, so the behaviour is known and deliberate. Being able to say "here is the worst case, here is the test that shows it" is a strong interview answer.

## Tests that wait for your code

Thirteen tests need `Merge` to exist. Each starts with:

```go
func requireMerge(t *testing.T) {
	probe := NewStore("probe", limitertest.NewFakeClock(start), 1)
	if !probe.Merge(Entry{...Count: 1}) {
		t.Skip("Store.Merge is not written yet: see merge.go")
	}
}
```

It tries one merge on an empty store. If nothing happens, the test **skips** itself, which is reported differently from pass or fail. When you write `Merge`, the probe succeeds and all thirteen start running, with nothing to switch on.

As with the token bucket, I checked the tests against a throwaway merge (all passed, also under the race detector) and then put the stub back.

## Partition testing without changing the wire format

The plan wants an endpoint that cuts a node off from a peer, to test a network partition. The obvious way, "drop packets from that peer", is not possible: a packet does not say who sent it, and the wire format is fixed.

But every *entry* names the node whose count it is. So `Block(peer)` does two things: never send to that peer, and ignore any entry *about* that peer, wherever it arrives from. When both sides block each other, neither can learn the other's counts by any route, including full sync. That is a complete partition, with the wire format untouched.

## Known limits

All from the plan, and all worth being able to explain:

- **Staleness:** a node's view lags by roughly half the gossip interval plus delivery time. Requests admitted elsewhere in that gap are invisible.
- **Burst from idle:** up to N times the limit in the worst case (the test above).
- **Partition:** each side enforces the full limit on its own.
- **Clocks:** window numbers come from each node's own clock. If clocks differ by a large part of a window, nodes disagree about which window a request is in.
- **Crash:** counts a node admitted but had not yet gossiped are lost with it.

## Check yourself

- Why does a node send its running total and not "5 more"?
- Walk through a duplicate packet, a late packet and a lost packet under the "keep the larger" rule.
- What do commutative, associative and idempotent mean, and which network fault does each one cover?
- What are `dirty` and `retransmit` for? Why does a merged entry become dirty?
- What is a uvarint, and why is there a version byte?
- Explain the "billion entries" attack and the one-line check that stops it.
- What does fuzzing find that hand-written tests do not?
- Why is full sync needed when gossip already exists?
- How does `Block` create a partition when packets carry no sender?
- Why does the gossip backend refuse the token bucket?
