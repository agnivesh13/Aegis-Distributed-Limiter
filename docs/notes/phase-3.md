# Phase 3: gossip, approximate and Redis-free

The same limit with no Redis: every request is decided from local memory, and gateways tell each other what they admitted in the background. The long explanations are in [learnings/](../../learnings/README.md), notes 09, 12 and 16.

## What was built

| Piece | Where | By |
| --- | --- | --- |
| Store: per-node counts, decision, dirty tracking, snapshot, sweep | `gossip/store.go` | Fable |
| Merge | `gossip/merge.go` | Fable |
| Wire format: encode, decode, packet splitting | `gossip/codec.go` | Fable |
| Node: gossip loop, memberlist delegate methods, block list, stats | `gossip/node.go` | Fable |
| `Transport` interface and an in-process fake network | `gossip/transport.go`, `gossip/sim_test.go` | Fable |
| memberlist transport | `gossip/memberlist.go` | Opus |
| `/debug/limiter`, `/debug/gossip/block`, Compose with the gossip backend | `internal/admin`, `cmd/gateway`, `deploy/` | Opus |

## Design choices

- **Nodes exchange running totals per node, never increments.** Merging keeps the larger value, so duplicates, reordering and loss cannot corrupt a count (a grow-only counter).
- **Merge accepts only the previous, current and next windows.** Older cannot matter; further ahead is a broken clock or a corrupt packet.
- **A changed count is sent for `retransmit` rounds, and merged news is relayed,** so it reaches peers the sender did not pick.
- **The node runs its own gossip loop** rather than using memberlist's broadcast queue, because interval and fanout are the experiment's variables.
- **Full sync over TCP** (memberlist push-pull) repairs whatever UDP lost.
- **The decoder trusts nothing:** every length is checked against the bytes present, and an entry count larger than the packet could hold is refused before memory is reserved.
- **Only the sliding window is supported.** Token bucket state goes up and down, so it has no merge rule.
- **A partition is made by ignoring entries about a blocked peer,** because packets carry no sender and the wire format is fixed.
- **`gossip_entries_merged_total` was added** so the bench runner can refuse a run in which nodes learned nothing from each other.

## What each test proves

| Test | Proves |
| --- | --- |
| `TestConformance` | A lone gossip node passes the sliding-window cases and refuses the token bucket |
| `TestMergeProperties` | On 200 random inputs each: order, duplication and grouping of entries do not change the result |
| `TestMergeTakesTheHigherCount`, `TestMergeIgnoresWindowsOutOfRange`, `TestMergedNewsIsRelayed` | The three rules of the merge, one by one |
| `FuzzDecode` | 2.5 million arbitrary inputs: no panic, and whatever decodes survives a round trip |
| `TestFanoutToEveryPeerAgreesInOneRound` | 5 nodes, fanout 4: one round and all agree |
| `TestFanoutOneAgreesAfterRoundsAndFullSync`, `TestLossyNetworkAgreesAfterFullSync` | Relay plus full sync converges, also with 30% of packets dropped, and loss never inflates a count |
| `TestRestartedNodeRelearnsItsOwnCount` | A node with empty memory gets its own count back from its peers |
| `TestBurstFromIdleAdmitsLimitOnEveryNode` | The known worst case: 300 admitted against 100 when a burst beats the first gossip round |
| `TestPartitionSplitsAndHeals` | Each half counts alone; full sync reunites them |
| `TestStartedNodesConvergeOnTheirOwn` | The real loop, with concurrent requests, under the race detector |
| `TestMemberlistTransport` | Three real memberlist nodes find each other, carry a packet and notice a departure |

The merge tests were themselves tested by injecting four wrong merges; each was caught.

## Done when

| Check | Result |
| --- | --- |
| All tests pass under `-race` | Passes |
| Identical totals on all three gateways within one second of the load stopping | Same total and same per-node counts (20, 23, 26) on all three |
| A measurable margin over the limit that shrinks from a 500 ms to a 10 ms interval | 21.6% at 500 ms, 0.8% at 10 ms |

Results: `bench/results/phase3-gossip-cluster.txt`.
