# 12. Running the cluster: containers, the load generator and what the checks showed

Files: [Dockerfile](../Dockerfile), [deploy/docker-compose.yml](../deploy/docker-compose.yml), [cmd/loadgen/main.go](../cmd/loadgen/main.go), [internal/limiter/gossip/memberlist.go](../internal/limiter/gossip/memberlist.go), [.github/workflows/ci.yml](../.github/workflows/ci.yml)

## Containers in one paragraph

A **container** is a program packaged with everything it needs to run (its files, its libraries, a minimal operating system userland) and run in isolation from other programs on the same machine. An **image** is the package; a container is a running copy of it. One image can run as many containers as you like.

## The Dockerfile: one image, every program

```dockerfile
FROM golang:1.27 AS build
...
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.22
COPY --from=build /out/ /app/
```

This is a **multi-stage build**. The first stage has the whole Go toolchain (about a gigabyte) and compiles. The second stage starts from a tiny Linux (Alpine, a few megabytes) and copies in only the finished programs. The final image carries no compiler.

- `./cmd/...` builds every program under `cmd/` in one go: gateway, upstream, loadgen, analyze, bench.
- `CGO_ENABLED=0` makes fully self-contained binaries that need no C libraries.
- `COPY go.mod go.sum` and `go mod download` come *before* `COPY . .`. Docker caches each step, and dependencies change far less often than code. Ordered this way, editing one `.go` file does not re-download every library.
- `USER app` runs the programs as an ordinary user, not root, so a bug in the gateway cannot do root's damage.

## Compose: the whole cluster in one file

Docker Compose starts several containers that can reach each other by name. Inside its network, the gateway reaches the upstream at `http://upstream:8081`.

### Scaling, and the name that resolves to many

```
docker compose up --scale gateway=3
```

starts three copies of the gateway service. Inside the network, the name `gateway` now resolves to **all three addresses**. Two parts of the project rely on that:

- The **load generator** looks the name up, gets three addresses, and sends requests to each in turn.
- The **gossip join** address is `gateway:7946`, so each new gateway finds the others with no list to maintain.

On the host side, three copies cannot all have port 8080, so the file publishes a range: `"8080-8084:8080"`. Each replica takes one free port from it.

### Profiles

Some services are only needed sometimes. A **profile** keeps them out of a normal `up`:

| Profile | Adds |
| --- | --- |
| `faults` | toxiproxy, to add delay between gateways and Redis |
| `observability` | Prometheus and Grafana |
| `loadgen` | The load generator, run on demand with `docker compose run` |

### Health checks

```yaml
healthcheck:
  test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9090/readyz"]
```

Docker calls this every 2 seconds. `docker compose up --wait` returns only when every container reports healthy. Without it, a test could start sending load to a gateway that has not finished starting, and the first second of data would be garbage.

## Continuous integration

[.github/workflows/ci.yml](../.github/workflows/ci.yml) runs on GitHub's Linux machines on every push: format check, vet, the unit tests under the race detector, the integration tests (GitHub's machines have Docker, so testcontainers works), and an image build. The race detector problem from note 07 does not exist there, because Linux runners have a 64-bit C compiler.

## The load generator

[cmd/loadgen](../cmd/loadgen/main.go) wraps Fable's open-loop scheduler ([10-measuring.md](10-measuring.md)) with the HTTP side.

```
loadgen -targets http://gateway:8080 -rate 200 -duration 30s -key k1 -out run.csv
```

| Flag | Meaning |
| --- | --- |
| `-rate`, `-duration` | Total requests per second, and for how long |
| `-key` or `-keys N` | One fixed API key, or N keys `key-0` to `key-N-1` |
| `-dist` | How requests spread over keys: `uniform`, `zipf` (a few keys get most traffic), `hot` (90% on one key) |
| `-shape burst` | Silence for `-idle`, then `-burst-rate` for `-burst-for`: scenario S7 |
| `-weights` | Uneven share of traffic per gateway |
| `-out` | Write every request to a CSV in the format the analyzer reads |

Two details worth copying:

**Everything random is decided before the run starts.** Which key and which gateway each request uses is worked out up front from a seed (`-seed`). The sending loop then only reads from a list. That keeps the hot loop cheap, and the same seed always gives the same traffic, so repeated runs are comparable.

**No locks around the results.** There is one slot per request, and request number `seq` writes only to slot `seq`. Different goroutines never touch the same slot, so no mutex is needed. (The race detector agrees.)

## The gossip transport: memberlist

The gossip node needed something real to find peers and carry packets. [memberlist.go](../internal/limiter/gossip/memberlist.go) wraps HashiCorp's `memberlist` library behind the `Transport` interface Fable defined.

Membership comes from the library: it pings members, marks unresponsive ones as suspect and then dead, and spreads that news. Our code only has to:

- list live members other than itself (`Peers`),
- send one packet to a member by name over UDP (`Send`),
- join through an address (`Join`) and leave politely (`Close`).

```go
var _ memberlist.Delegate = (*Node)(nil)
```

This line declares nothing at run time. It asks the *compiler* to check that `*Node` has every method memberlist calls. If someone renames one, the build fails here with a clear message, instead of memberlist silently not calling it.

**Joining is retried.** Replicas start in no particular order, so the first gateway's join attempt may find nobody. The gateway retries every second for up to a minute and logs when it finds a peer. A gateway that never finds one still works; it just limits on its own.

## What the checks showed

Every "Done when" check that could run was run in the real cluster. Results are saved under [bench/results/](../bench/results/).

### Phase 1, one gateway, memory backend

`curl` showed the rate-limit headers; a 300-request burst got exactly 100 through, then `429` with `Retry-After: 1`.

200 requests per second for 30 s against a limit of 100 per second: **2,971 admitted**, within 1% of 3,000. The analyzer's ideal for the same requests was also 2,971, so the gateway matched a perfect limiter exactly.

### Microbenchmarks

| | 1 CPU | 4 CPUs |
| --- | --- | --- |
| One hot key | 101 ns | 125 ns |
| 10,000 keys | 118 ns | 51 ns |

With one key, four cores are no faster than one: they all queue for one shard's lock. Spread over 10,000 keys, the 64 shards let four cores work at once and throughput rises 2.3×. This is the clearest picture of why the memory backend is sharded. Every call made zero memory allocations.

### Phase 2, three gateways on Redis

The sliding-window route let everything through, for the reason explained in [11-the-gateway.md](11-the-gateway.md). The token-bucket route, whose script exists, gave **319 admitted** across three gateways. Worked out by hand, a single exact limiter admits 20 from the full bucket plus 10 per second for the 29.995 s between the first and last request: 20 + 299 = 319. Three gateways together behaved exactly like one limiter.

Redis stopped for about 6 seconds under load:

| | fail_open | fail_closed |
| --- | --- | --- |
| Requests during the outage | proxied (1,670) | answered 503 (1,664) |
| Client latency p95 | 53 ms | 52 ms |
| After Redis returned | recovered by itself | recovered by itself |

The p95 of 52 ms is the 50 ms limiter deadline doing its job: no request waited longer than that for a dead Redis.

### Phase 3, three gateways on gossip

The cluster formed (each gateway saw 3 members), about 210 packets per gateway crossed during a 10 s run, and the block endpoint answered. The debug view showed each gateway knowing only its own count, which is exactly what an empty `Merge` means. The convergence checks wait for yours.

## Check yourself

- What does a multi-stage build save, and why are `go.mod` and `go.sum` copied before the rest of the code?
- How does one name, `gateway`, reach three containers, and which two parts of the project depend on it?
- What would go wrong without `--wait` and the health checks?
- Why is every key and target chosen before the load starts?
- Why do the load generator's goroutines need no lock to record results?
- What does `var _ memberlist.Delegate = (*Node)(nil)` do, and when does it do it?
- Why does the hot-key benchmark not get faster with more cores?
- Explain the 319 in the token-bucket run.
