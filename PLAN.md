# Distributed Rate Limiter / API Gateway in Go: Project Plan

Nov 14, 2026 · @Agnivesh

## Goal and scope

Build an HTTP gateway in Go that enforces per-client rate limits shared across several gateway instances, then measure what exactness costs. The same limiter runs on two backends: Redis with Lua scripts (exact) and a gossip protocol (approximate). The deliverable is working code plus a short report comparing over-admission against added latency.

**In scope**

- Reverse-proxy gateway with rate-limit middleware
- Two algorithms: token bucket and sliding window
- Three backends behind one interface: in-memory, Redis + Lua, gossip
- Load generator, metrics, Docker Compose cluster of 3 to 5 gateways
- Benchmark report with numbers and charts

**Out of scope for v1**

- Auth, TLS termination, service discovery, admin UI
- Production hardening and cloud deployment

**Resume line to earn** (fill X and Y from your own measurements): "Built a distributed rate-limiting API gateway in Go; compared a Redis + Lua exact limiter with a gossip-based approximate limiter across N nodes, measuring X% over-admission against Y ms p99 decision latency."

**Time estimate:** about 6 weeks part-time alongside placements. Phases 1 and 2 alone already make a usable project if time runs short.

## How to use this plan with Claude Code

Save this file as `PLAN.md` in the repo root and have Claude Code follow it one phase at a time.

**Rules for the coding agent**

1. Work on exactly one phase per session. Stop when its "Done when" checks pass and report the evidence.
2. Treat the "Technical spec" section as fixed. If a spec item has to change, update this file in the same commit.
3. Write limiter tests before limiter code. Run `go vet ./...` and `go test -race ./...` before every commit.
4. Add no dependency outside the tooling table, and build nothing from the ideas backlog, unless asked.
5. Commit in small steps; every commit builds and passes tests.
6. After each phase, write `docs/notes/phase-N.md`: what was built, the design choices made, and what each test proves.
7. Save every benchmark and experiment output under `bench/results/`. Numbers in the README and report must come from those files.
8. Use only cross-platform tooling: Go programs and `docker compose`. Python is allowed for the plotting script only. No bash-only scripts and no host tools such as `tc` or `iptables`.
9. `TASKS.md` tracks every task and who owns it. Tick a task there in the same commit that finishes it.
10. Every change gets a matching note in `learnings/`: what was built, why, and each problem met with its cause and fix, explained from the basics.

**Suggested split of the work**

The interview value comes from being able to defend the core, so write three pieces yourself and ask Claude Code to review them instead of generating them:

- The in-memory token bucket (Phase 1)
- The sliding-window Lua script (Phase 2)
- The gossip counter merge (Phase 3)

Let Claude Code handle scaffolding, Docker, metrics, the load generator and the bench runner.

## Architecture

Every gateway runs the same binary and checks each request with the limiter before proxying it. The configured backend decides how gateways share counts: one Redis call per request, or counters gossiped between gateways.

```text
                     +-----------------------+
                     |    Load generator     |
                     | round-robins requests |
                     +-----------+-----------+
                                 |
           +---------------------+---------------------+
           |                     |                     |
           v                     v                     v
  +-----------------+   +-----------------+   +-----------------+
  |    gateway 1    |<->|    gateway 2    |<->|    gateway 3    |
  | proxy + limiter |   | proxy + limiter |   | proxy + limiter |
  +--------+--------+   +--------+--------+   +--------+--------+
           |                     |                     |
           +----------+----------+----------+----------+
                      |                     |
                      v                     v
             +-----------------+   +-----------------+
             |      Redis      |   |    Upstream     |
             |   Lua scripts   |   |  echo service   |
             +-----------------+   +-----------------+

  <->   gossip backend: gateways exchange counters over UDP, Redis unused
  Redis is called only when the redis backend is selected
```

| Component | Where it lives | Responsibility |
| --- | --- | --- |
| Gateway | `cmd/gateway`, `internal/proxy` | Reverse proxy; runs the rate-limit middleware before forwarding |
| Limiter | `internal/limiter` | One interface, three backends: `memory`, `redis`, `gossip` |
| Redis | Official Docker image | Shared state for the exact backend; all limit logic in Lua |
| Gossip layer | `internal/limiter/gossip` | Membership through memberlist; counter exchange over UDP; full sync over TCP |
| Upstream | `cmd/upstream` | Dummy service that echoes requests, with optional artificial delay |
| Load generator | `cmd/loadgen` | Open-loop traffic at a fixed rate; logs every request to CSV |
| Analyzer | `cmd/analyze` | Computes accuracy and latency from a run's CSV |
| Bench runner | `cmd/bench` | Starts a cluster for a scenario, runs the load, collects results |
| Metrics | Prometheus, Grafana | Scrape each gateway's `/metrics` |

**Request path**

1. A request arrives on `:8080`; middleware assigns a request ID.
2. The route is the longest matching path prefix. The client is the `X-API-Key` header, or the client IP when the header is missing.
3. The middleware calls `Allow(ctx, "route:client", limit, cost)` on the configured backend, with a deadline.
4. Allowed: proxy to the upstream and add rate-limit headers. Rejected: `429` with `Retry-After`. Limiter error: apply the `on_error` policy.
5. Metrics record the decision, the decision latency and the upstream latency.

## Technical spec

These contracts are fixed so that every phase builds on the same foundation.

### Stack

| Item | Choice |
| --- | --- |
| Language | Go, latest stable release |
| Module path | `github.com/agnivesh13/Aegis-Distributed-Limiter` (matches the GitHub repo) |
| Redis | Version 7 or newer, single instance, persistence off |
| Runtime | Docker Compose; every component runs in a container |
| Logging | `log/slog`, JSON to stdout |
| Config | One YAML file, overridable by environment variables |

### Limiter contract

```go
package limiter

import (
	"context"
	"errors"
	"time"
)

type Algorithm string

const (
	TokenBucket   Algorithm = "token_bucket"
	SlidingWindow Algorithm = "sliding_window"
)

// Limit allows Rate requests per Period for one key.
type Limit struct {
	Algorithm Algorithm
	Rate      int           // requests per Period
	Period    time.Duration // window length, or the time to refill Rate tokens
	Burst     int           // token bucket capacity; 0 means Rate
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	Limit      int           // Rate, echoed for response headers
	Remaining  int           // whole requests still available, never negative
	RetryAfter time.Duration // zero when Allowed
	ResetAfter time.Duration // time until the full limit is available again
}

type Limiter interface {
	// Allow charges cost against key and reports the decision.
	// A rejected request consumes nothing.
	// The error is non-nil only when the backend cannot decide.
	Allow(ctx context.Context, key string, limit Limit, cost int) (Decision, error)
	Close() error
}

// Clock is injected into every backend so tests control time.
type Clock interface {
	Now() time.Time
}

var (
	ErrUnsupported = errors.New("limiter: algorithm not supported by this backend")
	ErrInvalidCost = errors.New("limiter: cost must be between 1 and the capacity")
)
```

- Capacity is `Burst` (or `Rate` when `Burst` is 0) for the token bucket, and `Rate` for the sliding window.
- The limiter key is `route:client`, for example `search:key-123`. Redis keys add a prefix: `rl:tb:` or `rl:sw:`.
- Config validation rejects `Rate < 1`, `Period <= 0` and `Burst < 0`.

### Backend and algorithm matrix

| Backend | Token bucket | Sliding window counter |
| --- | --- | --- |
| `memory` | Phase 1 | Phase 1 |
| `redis` | Phase 2 | Phase 2 |
| `gossip` | Not in v1: bucket state does not merge cleanly, so it returns `ErrUnsupported` | Phase 3 |

The head-to-head comparison in Phase 4 uses the sliding window counter, the one algorithm all three backends implement.

### HTTP contract

| Case | Response |
| --- | --- |
| Allowed | Upstream response plus `X-RateLimit-Limit`, `X-RateLimit-Remaining` and `X-RateLimit-Reset` (seconds) |
| Rejected | `429 Too Many Requests`, the same three headers, `Retry-After` in whole seconds rounded up, a small JSON body |
| Limiter error with `fail_open` | Request is proxied; `gateway_limiter_errors_total` goes up |
| Limiter error with `fail_closed` | `503 Service Unavailable` |
| No matching route | `404 Not Found` |
| Upstream unreachable | `502 Bad Gateway` |

- Every response carries `X-Gateway-ID`, so the load generator can attribute each request to an instance.
- Ports: `:8080` proxy; `:9090` admin; `:7946` gossip (TCP and UDP).
- Admin endpoints, never rate limited: `/healthz`, `/readyz`, `/metrics`, `/debug/limiter?key=...`, and with `DEBUG_FAULTS=1` on the gossip backend `POST` (block) or `DELETE` (unblock) `/debug/gossip/block?peer=...`.
- On start-up the gateway asks its backend once per route whether it can decide that route's algorithm, and logs an error for any it cannot; such a route would otherwise follow `on_error` on every request.
- Client identity: the `X-API-Key` header; if absent, the host part of `RemoteAddr`. `X-Forwarded-For` is used only when `trust_forwarded_for` is true.

### Configuration

```yaml
gateway:
  id: ""                      # default: hostname; env GATEWAY_ID
  listen: ":8080"
  admin_listen: ":9090"
  upstream: "http://upstream:8081"
  trust_forwarded_for: false

limiter:
  backend: memory             # memory, redis or gossip; env LIMITER_BACKEND
  on_error: fail_open         # fail_open or fail_closed; env LIMITER_ON_ERROR
  timeout: 50ms               # deadline for one Allow call

redis:
  addr: "redis:6379"          # env REDIS_ADDR
  pool_size: 64

gossip:
  bind_port: 7946
  join: ["gateway:7946"]      # DNS name that resolves to every gateway
  interval: 100ms             # env GOSSIP_INTERVAL
  fanout: 2                   # env GOSSIP_FANOUT
  retransmit: 3               # rounds a changed entry keeps being sent
  max_packet_bytes: 1200
  full_sync_interval: 5s

routes:                       # longest path_prefix wins
  - name: api
    path_prefix: /api/
    cost: 1
    limit: { algorithm: sliding_window, rate: 100, period: 1s }
  - name: search
    path_prefix: /api/search
    cost: 5
    limit: { algorithm: token_bucket, rate: 50, period: 1s, burst: 100 }
```

The gateway validates the config on startup and exits with a clear message on any invalid value. Environment variables override the file, so the bench runner can change one variable per run. Besides the ones marked above: `GATEWAY_UPSTREAM`, `GOSSIP_JOIN`, `GOSSIP_RETRANSMIT`, `GOSSIP_FULL_SYNC_INTERVAL`.

`deploy/gateway-bench.yaml` holds the configuration for experiments: a single sliding-window route, the one algorithm every backend implements. Compose picks the file through `GATEWAY_CONFIG`.

### Metrics

| Metric | Type | Labels |
| --- | --- | --- |
| `gateway_requests_total` | counter | `route`, `decision` (allowed, rejected, error) |
| `gateway_limiter_decision_seconds` | histogram | `backend`, `algorithm` |
| `gateway_upstream_seconds` | histogram | `route` |
| `gateway_limiter_errors_total` | counter | `backend` |
| `limiter_tracked_keys` | gauge | `backend` |
| `gossip_packets_sent_total` | counter | none |
| `gossip_bytes_sent_total` | counter | none |
| `gossip_delivery_seconds` | histogram | none |
| `gossip_members` | gauge | none |
| `gossip_entries_merged_total` | counter | none |

- Decision histogram buckets: exponential, starting at 10 microseconds, factor 1.5, 24 buckets (up to about 110 ms).
- `gossip_delivery_seconds` is the receiver's clock minus the packet's send timestamp. It is valid here because all containers share one host clock.
- `gossip_entries_merged_total` counts received counts that told a node something new. If it stays at zero across a cluster, the nodes are not learning from each other, and `cmd/bench` refuses to record the run.

### Compose services

| Service | Image | Notes |
| --- | --- | --- |
| `gateway` | Built from the repo `Dockerfile` | Scaled with `docker compose up --scale gateway=N`; CPU limit 1.0 each |
| `upstream` | Same image, different command | One instance |
| `redis` | `redis:7-alpine` or newer | Started with persistence off |
| `toxiproxy` | `ghcr.io/shopify/toxiproxy` | Profile `faults`; sits between gateways and Redis to add delay |
| `loadgen` | Same image, different command | Run on demand; writes CSV into a mounted `bench/results/` |
| `prometheus`, `grafana` | Official images | Profile `observability`; Prometheus on host port 9099, Grafana on 3000 |

- One multi-stage `Dockerfile` builds every binary into one small image; each service selects its binary with `command`.
- Inside the Compose network the name `gateway` resolves to every replica. The load generator and the gossip join both rely on that.
- Replicas cannot share one host port. For manual `curl` testing, publish a host port range such as `8080-8084:8080`.

## Algorithms

Implement token bucket and sliding-window counter as the two required algorithms; the others are cheap additions once the interface exists.

| Algorithm | State per key | Burst behaviour | Where it is wrong |
| --- | --- | --- | --- |
| Token bucket | Tokens left, last refill time | Allows bursts up to bucket capacity | Exact on one node; needs atomic read-compute-write when shared |
| Fixed window | One counter per window | Up to 2x the limit across a window boundary | Boundary burst; build it only as a baseline |
| Sliding window log | Timestamp of every request (Redis sorted set) | No boundary burst | Exact, but memory grows with request rate |
| Sliding window counter | Current and previous window counts | Smooths the boundary | Approximate: assumes the previous window's requests were evenly spread |
| GCRA (stretch) | One timestamp | Same as token bucket | Exact; harder to explain, compact to store |

**Design notes**

- Token bucket refills lazily: compute tokens from elapsed time on each request, no background timer.
- Sliding window counter estimate: `current + previous * (1 - elapsed / window)`.
- In Redis, read the time with `TIME` inside the Lua script, so gateway clock skew cannot affect decisions.
- Every key gets a TTL, so idle clients cost no memory.
- Every limiter takes an injected clock, so tests never sleep.

### Token bucket: exact behaviour

State per key: `tokens` (a float) and `ts` (time of the last update). `rate = Rate / Period` in tokens per second; `capacity = Burst`, or `Rate` when `Burst` is 0.

1. New key: `tokens = capacity`, `ts = now`.
2. Refill: `tokens = min(capacity, tokens + (now - ts) * rate)`. Treat a negative elapsed time as 0.
3. If `tokens >= cost`, subtract `cost` and allow. Otherwise reject with `RetryAfter = (cost - tokens) / rate`.
4. Store `tokens` and `ts = now` in both cases.
5. `Remaining = floor(tokens)`; `ResetAfter = (capacity - tokens) / rate`.
6. Expire the key after `capacity / rate` idle seconds, because a missing key and a full bucket are equivalent.

### Sliding window counter: exact behaviour

State per key: `idx` (window number), `cur` and `prev` (counts). `W = Period`, `limit = Rate`.

1. `idx_now = floor(now / W)` and `elapsed = now - idx_now * W`.
2. Rotate the stored state: same window, keep it; one window old, `prev = cur` and `cur = 0`; anything else, both are 0.
3. `estimate = cur + prev * (1 - elapsed / W)`.
4. If `estimate + cost <= limit`: `cur += cost`, store `(idx_now, cur, prev)`, allow. Otherwise reject and store nothing.
5. `Remaining = max(0, floor(limit - estimate))`, using the estimate after the charge.
6. `RetryAfter` when rejected: if `cur + cost <= limit`, wait for the previous window's weight to decay, `W * (1 - (limit - cur - cost) / prev) - elapsed`. Otherwise wait for the end of the current window, `W - elapsed`; that value is a lower bound.
7. `ResetAfter`: `2 * W - elapsed` if `cur > 0`, else `W - elapsed` if `prev > 0`, else 0.
8. Expire the key after `2 * W` without a write.

### Redis Lua scripts

Each script touches one key, reads the time from Redis and returns four integers: `{allowed, remaining, retry_after_ms, reset_after_ms}`. The optional last argument `now_ms` exists only so tests can control time; production calls omit it. `Period` must be at least 1 ms because the scripts work in whole milliseconds.

`internal/limiter/redis/scripts/token_bucket.lua`

```lua
-- KEYS[1] = rl:tb:<route>:<client>
-- ARGV    = rate_per_sec, capacity, cost, [now_ms]
local rate     = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local cost     = tonumber(ARGV[3])

local now = tonumber(ARGV[4])
if now == nil then
  local t = redis.call('TIME')
  now = t[1] * 1000 + math.floor(t[2] / 1000)
end

local state  = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])
if tokens == nil then
  tokens = capacity
  ts = now
end

local elapsed = math.max(0, now - ts)
tokens = math.min(capacity, tokens + elapsed * rate / 1000)

local allowed = 0
local retry_ms = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost
else
  retry_ms = math.ceil((cost - tokens) * 1000 / rate)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity * 1000 / rate) + 1000)

local reset_ms = math.ceil((capacity - tokens) * 1000 / rate)
return {allowed, math.floor(tokens), retry_ms, reset_ms}
```

`internal/limiter/redis/scripts/sliding_window.lua`

```lua
-- KEYS[1] = rl:sw:<route>:<client>
-- ARGV    = limit, window_ms, cost, [now_ms]
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local cost   = tonumber(ARGV[3])

local now = tonumber(ARGV[4])
if now == nil then
  local t = redis.call('TIME')
  now = t[1] * 1000 + math.floor(t[2] / 1000)
end

local idx     = math.floor(now / window)
local elapsed = now - idx * window

local state = redis.call('HMGET', KEYS[1], 'idx', 'cur', 'prev')
local sidx  = tonumber(state[1])
local cur   = tonumber(state[2]) or 0
local prev  = tonumber(state[3]) or 0
if sidx == nil or sidx < idx - 1 or sidx > idx then
  cur = 0
  prev = 0
elseif sidx == idx - 1 then
  prev = cur
  cur = 0
end

local estimate = cur + prev * (1 - elapsed / window)

local allowed = 0
local retry_ms = 0
if estimate + cost <= limit then
  allowed = 1
  cur = cur + cost
  estimate = estimate + cost
  redis.call('HSET', KEYS[1], 'idx', idx, 'cur', cur, 'prev', prev)
  redis.call('PEXPIRE', KEYS[1], window * 2)
elseif cur + cost <= limit then
  retry_ms = math.ceil(window * (1 - (limit - cur - cost) / prev) - elapsed)
else
  retry_ms = window - elapsed
end

local remaining = math.max(0, math.floor(limit - estimate))
local reset_ms = 0
if cur > 0 then
  reset_ms = 2 * window - elapsed
elseif prev > 0 then
  reset_ms = window - elapsed
end
return {allowed, remaining, retry_ms, reset_ms}
```

Calling a script from Go:

```go
//go:embed scripts/sliding_window.lua
var slidingWindowSrc string

var slidingWindow = redis.NewScript(slidingWindowSrc)

func (b *Backend) runSliding(ctx context.Context, key string, l limiter.Limit, cost int) ([]int64, error) {
	return slidingWindow.Run(ctx, b.rdb, []string{"rl:sw:" + key},
		l.Rate, l.Period.Milliseconds(), cost).Int64Slice()
}
```

- `Script.Run` sends `EVALSHA` and falls back to `EVAL` when Redis has not cached the script.
- Redis truncates Lua numbers to integers in replies, so the scripts return milliseconds, never fractional seconds.
- For the token bucket, pass the rate as `float64(l.Rate) / l.Period.Seconds()`.
- Why Lua: with `GET`, compute in Go, then `SET`, two gateways can read the same state and both allow. A script runs atomically inside Redis in one round trip.
- Treat both scripts as a starting point: the conformance tests in Phase 2 are the authority on correctness.

## Gossip limiter design

The gossip backend decides every request from local memory and repairs its view of the other nodes in the background. It implements the sliding window counter only.

### State

For each key and each window number, a node stores one count per node: a grow-only counter (G-Counter).

```go
type windowCounts struct {
	perNode map[string]uint64 // node ID -> requests that node admitted in this window
	total   uint64            // cached sum of perNode
	dirty   map[string]int    // node ID -> gossip rounds this entry still has to be sent
}

type keyState struct {
	windowMs int64
	windows  map[int64]*windowCounts // window number -> counts; current and previous only
}
```

The store is a sharded map of `keyState` (64 shards, one mutex each), the same layout as the memory backend.

### Decision (request path, no network)

1. `idx = floor(now / W)`; drop windows older than `idx - 1`.
2. `estimate = total(idx) + total(idx - 1) * (1 - elapsed / W)`.
3. If `estimate + cost <= limit`: add `cost` to `perNode[self]` and to `total` for window `idx`, set `dirty[self] = retransmit`, allow.
4. Otherwise reject. `Remaining`, `RetryAfter` and `ResetAfter` follow the sliding window rules.

### Merge (on receiving an entry)

An entry is `(key, window_ms, idx, node, count)`.

1. Ignore it if `idx` is older than the local previous window or more than one window ahead.
2. If `count` is greater than the stored value for that node: raise `total` by the difference, store `count`, set `dirty[node] = retransmit` so the news is relayed.
3. Otherwise do nothing.

Taking the maximum makes merge commutative, associative and idempotent, so duplicated, reordered or lost packets cannot corrupt a count. The rule also applies to a node's own entry: after a restart it re-learns its own count from its peers.

### Dissemination

- **Membership and transport:** hashicorp/memberlist. Node name is the gateway ID; join through the `gossip.join` addresses.
- **Gossip loop:** every `interval`, collect all dirty entries and decrement their counters, encode them into packets of at most `max_packet_bytes`, pick `fanout` random live peers, and send every packet to each with `SendBestEffort` (UDP).
- **Receive:** `Delegate.NotifyMsg` decodes and merges. Copy the buffer before keeping any part of it.
- **Full sync:** `Delegate.LocalState` returns every live entry and `MergeRemoteState` merges it. memberlist runs this push-pull over TCP on join and every `full_sync_interval` (its `PushPullInterval`), which repairs anything UDP lost.
- **Unused hooks:** `GetBroadcasts` and `NodeMeta` return nil. Running our own loop gives direct control of interval and fanout, which are experiment variables.
- **Dead nodes:** nothing to clean up. Their counts are real admissions and expire with the window.

### Wire format

Binary, written with `encoding/binary` varints:

```text
packet := version:u8  type:u8  sent_unix_ms:uvarint  n:uvarint  entry{n}
entry  := key:bytes  window_ms:uvarint  idx:uvarint  node:bytes  count:uvarint
bytes  := len:uvarint  data
```

`type` is 1 for a gossip packet and 2 for a full-sync state. Packets with an unknown version are dropped and counted.

### Known limits (state them in the report)

- **Staleness:** a node's view of its peers lags. The lag is about half the gossip interval plus delivery time when `fanout` covers every peer, and grows as `fanout` shrinks. Requests admitted elsewhere during the lag are invisible, which causes over-admission.
- **First-order prediction to test:** under steady overload spread evenly over N nodes, over-admission is about `((N - 1) / N) * lag / W` of the limit. With N = 3, W = 1 s and a lag of 100 ms, that is about 7%. This is a rough model; Phase 4 checks it.
- **Burst from idle:** if a burst reaches all nodes within one lag, each can admit up to the full limit. Worst case is N times the limit in that window.
- **Partition:** each side enforces the limit on its own.
- **Clocks:** window numbers come from each node's wall clock. Skew that is a large fraction of W breaks alignment. It does not arise on one laptop; across machines it would.
- **Crash:** admissions a node had not yet gossiped are lost.

## Build plan

Six phases, each ending in something that runs. Stop after any phase and the project is still presentable.

### Phase 0: Warm-up and scaffolding (3 to 4 days)

**Goal:** you can read and write basic concurrent Go, and the repo skeleton builds.

**Your warm-up (by hand, not Claude Code)**

- [ ] A Tour of Go, including the concurrency section
- [ ] A worker pool with goroutines and channels, cancelled through `context`
- [ ] A counter shared by 100 goroutines, written three ways: unprotected, `sync.Mutex`, `sync/atomic`; run each with `go test -race`
- [ ] An HTTP server with one logging middleware and a table-driven test for it

**Scaffolding**

- [ ] `go mod init`, the directory tree from "Repo layout", `.gitignore`, a `README.md` stub listing the build, test and run commands
- [ ] `cmd/upstream`: echo server on `:8081` with a `-delay` flag and `/healthz`
- [ ] Multi-stage `Dockerfile`; `deploy/docker-compose.yml` with only `upstream`
- [ ] GitHub Actions workflow running `go vet ./...` and `go test -race ./...`

**Done when**

- `go build ./...` and `go vet ./...` succeed.
- `docker compose -f deploy/docker-compose.yml up upstream` answers `curl localhost:8089/hello` (the upstream listens on 8081 inside the network; on the host it is published on 8089, outside the gateways' port range).
- Your unprotected counter fails the race detector and the other two pass.

### Phase 1: Single-node gateway (week 1)

**Goal:** one gateway enforces limits from memory, and a test suite defines the behaviour every later backend must match.

**Tasks**

- [ ] `internal/limiter/limiter.go`: the contract from "Technical spec", plus a `SystemClock`
- [ ] `internal/limiter/limitertest`: a `FakeClock` with `Advance(d)`, and one exported conformance suite `Run(t, factory)` that every backend must pass
- [ ] `internal/limiter/memory`: both algorithms; 64 shards chosen by FNV-1a hash of the key, one mutex per shard; a janitor goroutine that removes expired keys; `Close` stops it
- [ ] `internal/config`: YAML loading, environment overrides, validation
- [ ] `internal/proxy`: `httputil.ReverseProxy` behind a middleware chain in this order: recover, request ID, metrics, route match, client identity, rate limit
- [ ] `internal/metrics`: the collectors from the spec, served on the admin port
- [ ] `cmd/gateway`: wiring, plus graceful shutdown on SIGINT and SIGTERM with a 10 s drain
- [ ] `cmd/loadgen` v0: flags `-targets`, `-rate`, `-duration`, `-key`; open-loop sender; prints allowed, rejected, errors and p50, p95, p99
- [ ] Compose: add `gateway` with the memory backend

**Conformance cases (fake clock)**

- Token bucket: a full bucket allows exactly `capacity` requests, then rejects.
- Token bucket: after `1 / rate` seconds exactly one more request is allowed.
- Token bucket: tokens never exceed `capacity` after a long idle period.
- Token bucket: a request with `cost = 5` consumes 5 tokens; a rejected request consumes none.
- Token bucket: `RetryAfter` equals the time until enough tokens exist.
- Sliding window: exactly `limit` requests are allowed inside one window.
- Sliding window: after a full previous window, half-way through the next window exactly `limit / 2` are allowed.
- Sliding window: after two idle windows the full limit is available again.
- Both: keys are independent of each other.
- Both: `cost` above the capacity returns `ErrInvalidCost` and changes nothing.
- Both: 50 goroutines sending 10,000 requests at a frozen clock admit exactly the capacity, under `-race`.

**Benchmarks**

- [ ] `BenchmarkAllow` for both algorithms: single goroutine, then `b.RunParallel` with 1 key and with 10,000 keys
- [ ] Save the output to `bench/results/phase1-microbench.txt`

**Done when**

- `go test -race ./...` passes.
- `curl` against the gateway shows the rate-limit headers, then `429` with `Retry-After` once the limit is exceeded.
- Load generator at 200 req/s against a 100 req/s sliding-window limit for 30 s: the admitted total is within 2% of 3,000.
- `/metrics` shows the request counters and the decision-latency histogram moving.

### Phase 2: Redis + Lua, exact and shared (week 2)

**Goal:** any number of gateways enforce one exact shared limit.

**Tasks**

- [ ] `internal/limiter/redis/scripts/`: the two Lua scripts from "Algorithms"
- [ ] `internal/limiter/redis`: go-redis client; scripts embedded with `go:embed` and run through `redis.NewScript`; the four returned integers mapped to `Decision`; a per-call deadline from `limiter.timeout`
- [ ] Error policy in the rate-limit middleware: `fail_open` and `fail_closed`
- [ ] `/readyz` reports not ready while Redis is unreachable
- [ ] A naive backend used only in tests (`GET`, compute in Go, `SET`), to demonstrate the lost-update race
- [ ] Compose: add `redis`; make `gateway` scalable; add `toxiproxy` under the `faults` profile
- [ ] Load generator: resolve the `gateway` DNS name to all replicas and round-robin across them

**Tests**

- The conformance suite runs against a real Redis started by testcontainers-go (build tag `integration`), with time injected through the `now_ms` argument.
- Exactness: three limiter instances sharing one Redis, 10,000 concurrent requests at a frozen time, admit exactly `limit`.
- The same test against the naive backend is expected to admit more than `limit`. Record the number for the report.
- With Redis stopped, `Allow` returns an error within the timeout. After Redis returns, calls succeed without restarting anything.

**Done when**

- `go test -race -tags integration ./...` passes.
- 3 gateways, 200 req/s offered against a 100 req/s limit, 30 s: the admitted total is inside the same 2% band as Phase 1.
- Redis stopped for 5 s under load: with `fail_open` every request is proxied and the error counter rises; with `fail_closed` clients get `503`. Both recover unaided.

### Phase 3: Gossip, approximate and Redis-free (weeks 3 to 4)

**Goal:** the same limit with no Redis, decided from local memory.

**Tasks, in build order (each is testable before the next)**

- [ ] `gossip/store.go`: state, decision and merge from "Gossip limiter design"; pure code, no network, fake clock
- [ ] `gossip/codec.go`: encode and decode; split entries into packets of at most `max_packet_bytes`
- [ ] `gossip/transport.go`: a small interface with `Send(peer, packet)` and `Peers()`; one memberlist implementation and one in-process fake
- [ ] `gossip/node.go`: the gossip loop, the memberlist delegate methods, metrics, and a `Close` that leaves the cluster
- [ ] Admin endpoint `/debug/limiter?key=...`: the node's per-node counts for a key, as JSON
- [ ] Admin endpoint `/debug/gossip/block?peer=...`, enabled only when `DEBUG_FAULTS=1`: drop packets to and from a peer, for partition tests
- [ ] Compose: run with `LIMITER_BACKEND=gossip`; gateways join through the `gateway` DNS name

**Tests**

- Store: the sliding-window conformance cases pass on a single node.
- Merge properties, using randomly generated entries: applying them in any order, or applying them twice, gives the same state.
- Codec: round trip, plus a fuzz test that `decode` never panics on arbitrary bytes.
- Simulation: 5 stores on the fake transport with one fake clock. With fanout 4, all totals agree after one round.
- Simulation: with fanout 1, totals agree after the gossip rounds followed by one full-sync pass.
- Loss: with 30% of fake packets dropped, totals still agree after a full-sync pass.
- Restart: a node that loses its state re-learns its own count from its peers.

**Done when**

- All tests pass under `-race`.
- 3 gateways without Redis: within one second of the load stopping, `/debug/limiter` shows identical totals on all three.
- Under steady 2x load, the admitted total exceeds the limit by a measurable margin, and the margin shrinks when `GOSSIP_INTERVAL` drops from 500 ms to 10 ms.

### Phase 4: Measure (week 5)

**Goal:** every scenario in "Experiment design" runs from one command and produces a row of results.

**Tasks**

- [ ] `cmd/loadgen` v1: key count and distribution (`-keys`, `-dist` with values uniform, zipf or hot), traffic shape (`-shape` with values steady or burst), per-gateway weights, CSV output
- [ ] CSV columns: `seq, key, send_unix_ns, latency_ns, status, gateway_id`
- [ ] The load generator also counts late sends: requests that left more than 5 ms after their scheduled time
- [ ] `cmd/analyze`: reads a run's CSV and writes one row to `bench/results/summary.csv` with the admitted total, ideal total, over-admission %, false rejects %, peak admitted in any window of length W, and latency percentiles
- [ ] `cmd/bench`: for a named scenario, set the environment, run `docker compose up --scale gateway=N`, wait for `/readyz`, run the load generator, inject faults on schedule, scrape each gateway's `/metrics`, tear down
- [ ] `bench/scenarios.yaml`: the scenario matrix
- [ ] `bench/plot.py`: the two charts, drawn from `summary.csv`
- [ ] Run every scenario 3 times; keep raw CSVs out of git, keep `summary.csv` and the charts in git

**Done when**

- `go run ./cmd/bench -scenario S1` produces a summary row end to end with no manual steps.
- The noise-floor scenario (S0) shows over-admission under 1%. If it does not, fix the measurement before trusting any other number.
- `bench/results/` holds `summary.csv` and both charts.

### Phase 5: Write-up and polish (week 6)

**Goal:** a stranger can understand the result in two minutes and reproduce it in ten.

**Tasks**

- [ ] `README.md`: one-paragraph pitch, the architecture diagram, quick start, results table, both charts, limitations
- [ ] `docs/report.md`: question, setup, method, results per scenario, the first-order prediction against the measurement, limitations, what you would change
- [ ] A 2-minute screen recording: start the cluster, show `429` responses, switch backend, show the dashboard
- [ ] Resume bullets and a 60-second spoken summary, both built on measured numbers
- [ ] Tag `v1.0`

**Done when**

- Someone who has never seen the repo can clone it, run one command, and reproduce one row of the results table.
- Every number in the README and the report traces to a file in `bench/results/`.

## Experiment design: accuracy against latency

The question to answer: how much over-admission does the gossip limiter accept in exchange for removing the Redis round trip from every request?

**What to measure**

| Metric | Definition | How |
| --- | --- | --- |
| Over-admission | Requests allowed beyond what an ideal limiter permits, as % of the limit | Replay the load generator's log through a single-node reference limiter offline |
| False rejects | Requests rejected that the ideal limiter would allow | Same replay |
| Decision latency | Time inside `Allow`, p50 / p95 / p99 | Histogram in the gateway |
| End-to-end latency | Client-side request time, p50 / p95 / p99 | Load generator |
| Throughput ceiling | Max req/s per gateway before latency degrades | Step the load up |
| Coordination cost | Redis ops/s, or gossip bytes/s per node | Redis `INFO`, counter on the gossip sender |

**What to vary**

| Variable | Values to try |
| --- | --- |
| Backend | In-memory (1 node baseline), Redis + Lua, gossip |
| Gateway count | 1, 3, 5 |
| Offered load against the limit | 0.5x, 1x, 2x, 10x |
| Gossip interval | 10 ms, 50 ms, 100 ms, 500 ms |
| Gossip fanout | 1, 2, 4 |
| Key distribution | One hot key, uniform, skewed (Zipf) |
| Traffic shape | Steady, sudden burst |
| Network delay to Redis | 0 ms, 1 ms, 5 ms (inject with toxiproxy) |

Do not run the full cross product. Fix a default for each variable and sweep one at a time.

**Hypotheses to confirm or kill**

- Redis stays exact at any node count, and its decision latency tracks the network round trip.
- Gossip decision latency stays near the in-memory baseline.
- Gossip over-admission grows with gossip interval, node count and burstiness, because each node decides on counts that are one interval stale.
- Under steady load below the limit, both backends behave the same.

**Fault runs**

- Kill one gateway mid-run: does the gossip cluster lose that node's counts?
- Restart Redis: what do clients see under each fail policy?
- Partition the gossip cluster in two: each half should over-admit up to the full limit.

**Honesty note for the report:** everything runs on one laptop under Docker, so the load generator and gateways compete for CPU. Report relative differences and medians of 3 runs, not absolute numbers.

**Charts for the report**

1. Over-admission % against gossip interval, one line per node count
2. p99 decision latency per backend, with injected Redis delay on the x-axis

### Scenario matrix

Defaults unless a row says otherwise: 3 gateways, sliding window counter, limit 100 req/s per key, 10 keys with uniform traffic, offered load 2x the limit (2,000 req/s in total), gossip interval 100 ms, fanout 2, a 5 s warm-up followed by a 30 s measured run, 3 repetitions.

| ID | Question | Backends | What changes |
| --- | --- | --- | --- |
| S0 | How noisy is the measurement itself? | memory | 1 gateway |
| S1 | Head-to-head at the defaults | redis, gossip | Nothing |
| S2 | Effect of cluster size | redis, gossip | 1, 3, 5 gateways |
| S3 | Effect of load | redis, gossip | 0.5x, 1x, 2x, 10x the limit |
| S4 | Effect of gossip interval | gossip | 10, 50, 100, 500 ms; at 3 and at 5 gateways |
| S5 | Effect of fanout | gossip | 1, 2, 4; at 5 gateways |
| S6 | Effect of Redis network delay | redis | 0, 1, 5 ms added through toxiproxy |
| S7 | Burst from idle | redis, gossip | 5 s of silence, then 10x load for 1 s |
| S8 | Skewed keys | redis, gossip | 1,000 keys with a Zipf distribution |
| S9 | Throughput ceiling | memory, redis, gossip | Raise total load in steps until p99 doubles |
| F1 | Gateway crash | redis, gossip | Kill one gateway at t = 10 s |
| F2 | Redis outage | redis | Stop Redis for 5 s at t = 10 s; run both error policies |
| F3 | Network partition | gossip | 4 gateways split 2 + 2 for 10 s through the debug block endpoint |

### Measurement rules

- **Open loop:** the load generator sends on a fixed schedule and never waits for responses, so slow responses cannot hide load.
- **Warm-up:** discard the first 5 s of every run.
- **Isolation:** 1 CPU per gateway and 2 for the load generator. Close other heavy apps, and pause 10 s between runs so the laptop does not heat up and throttle.
- **Repetition:** 3 runs per point; report the median and the range.
- **Sizing:** if late sends or client errors exceed 1%, the laptop cannot sustain the scenario. Lower the total rate and note it in the report.
- **Same path:** for S6, route Redis traffic through toxiproxy at every delay value, including 0 ms, so the proxy's own overhead cancels out.

### How accuracy is computed

1. Sort the run's CSV by send time. The measured window is everything after the warm-up.
2. Ideal total: replay every request for a key through the memory limiter, with the fake clock set to each send time, and count what it admits inside the measured window. The replay starts at the first request, warm-up included, so the reference enters the measured window holding the same previous-window counts the gateways did.
3. Over-admission % = `max(0, admitted - ideal) / ideal * 100`, per key and overall.
4. False rejects % = `max(0, ideal - admitted) / ideal * 100`.
5. Peak check: the largest number of admitted requests in any interval of length W, divided by the limit.

Send time stands in for arrival time at the limiter, which adds a small error. Scenario S0 measures that error, and every other result should be read against it.

## Ideas backlog

Everything worth noting that is not in the core plan. The first three are the highest value for interviews.

**Top picks**

- **Hybrid token leasing:** each gateway takes a batch of tokens from Redis (say 10) and spends them locally. One Redis trip per 10 requests. This is a third point on the accuracy-latency curve, between exact and gossip.
- **Hot-key handling:** detect keys with high traffic and gossip them more often, or route them to Redis while cold keys stay local.
- **Failure policy as a config switch:** fail open, fail closed, or degrade to a local `limit / N`, with a circuit breaker around Redis.

**Limiter features**

- GCRA and leaky bucket as extra algorithms
- Concurrency limit: cap in-flight requests per client, not requests per second
- Hierarchical limits: per user, per organisation and global, all checked in one Lua call
- Cost-weighted requests: an expensive endpoint spends more tokens
- Per-route and per-tier limits, hot-reloaded from config without restart
- Long-window quotas (daily, monthly) alongside per-second limits
- Shadow mode: log what would be rejected without rejecting, for safe rollout

**Redis side**

- Pipelining or batching decisions for several keys
- Redis Cluster with hash tags so one client's keys land on one shard
- Behaviour during a Redis failover: are counters lost, and does it matter?
- Compare Lua against `WATCH`/`MULTI`/`EXEC` with retries, to show why Lua wins

**Gossip side**

- Write your own SWIM-style membership and failure detector instead of memberlist
- Delta-state gossip: send only what changed since the last round
- Anti-entropy pass: periodic full-state sync to repair missed messages
- Static quota split (`limit / N` per node) as the simplest baseline, then quota rebalancing between busy and idle nodes
- Consistent hashing: each key has an owner node, and other gateways forward the decision to it (exact, no Redis, one hop)
- Raft-replicated counters with an embedded Raft library: exact without Redis, far stretch

**Gateway features**

- Upstream load balancing: round robin, least connections
- Health checks, timeouts, retries with backoff, circuit breaker per upstream
- JWT or API-key auth feeding the rate-limit key
- Request IDs and OpenTelemetry tracing across gateway and upstream
- Graceful shutdown that drains in-flight requests
- Admin endpoint to inspect a client's current bucket

**Operations and testing**

- Grafana dashboard: allowed vs rejected, latency, gossip convergence time
- pprof CPU and allocation profiles, with one optimisation documented before and after
- Fuzz tests on the limiter; property test that the limiter never admits more than limit + burst on one node
- Deterministic simulation: run the gossip cluster in one process with a fake network and fake clock
- Deploy to a local Kubernetes cluster (kind) with a headless service for peer discovery

**Presentation**

- Blog post: "Exact vs approximate distributed rate limiting, measured"
- A live demo page showing counters converging across nodes
- A "what I would do differently" section in the README

## What this teaches, and what to say in interviews

The project covers the standard "design a rate limiter" question end to end, with measurements to back each trade-off.

| Area | Concepts you will have used |
| --- | --- |
| Go | Goroutines, channels, `context` cancellation, mutexes and atomics, interfaces, `net/http` middleware, `go:embed`, table-driven tests, benchmarks, race detector, pprof |
| Distributed systems | Check-then-act races and atomicity, clock skew, consistency vs availability, eventual consistency, CRDT counters, gossip and membership, failure detection, tail latency, fail-open vs fail-closed |
| Engineering practice | Designing behind an interface, experiment design, observability, fault injection, writing up results |

**Questions to be able to answer from your own results**

- Why a Lua script and not `GET` then `SET`? Two gateways can read the same count and both allow. Lua runs the read, compute and write atomically in one round trip.
- What happens when Redis is down? State your policy and what you observed.
- How far over the limit can the gossip version go, and what controls it? Answer with your measured curve.
- Which would you ship? Exact for billing or abuse limits; approximate where latency matters and a small overshoot is harmless.
- How does it scale to millions of keys? TTLs, memory per key for each algorithm, sharding.
- What about clock skew between gateways? Redis-side time for the exact path; window-aligned counters for gossip.
- What does a network partition do to each design?

## Repo layout, tooling and risks

```text
Aegis-Distributed-Limiter/
  PLAN.md                       this file
  TASKS.md                      task tracker: who does what, and what is done
  learnings/                    study notes: the code explained from the basics
  README.md
  Dockerfile                    multi-stage; every binary in one image
  go.mod
  .github/workflows/ci.yml      go vet and go test -race
  cmd/
    gateway/                    the gateway
    upstream/                   dummy echo service
    loadgen/                    open-loop load generator
    analyze/                    accuracy and latency from a run's CSV
    bench/                      scenario runner; drives docker compose
  internal/
    limiter/
      limiter.go                contract: Limit, Decision, Limiter, Clock
      limitertest/              fake clock and the conformance suite
      memory/                   Phase 1
      redis/                    Phase 2
        scripts/                token_bucket.lua, sliding_window.lua
      gossip/                   Phase 3: store, codec, transport, node
    proxy/                      reverse proxy and middleware
    config/                     YAML, env overrides, validation
    metrics/                    Prometheus collectors
  deploy/
    docker-compose.yml
    gateway.yaml                default gateway config
    prometheus.yml
    grafana/                    dashboard JSON
  bench/
    scenarios.yaml
    plot.py
    results/                    summary.csv, charts, microbench output
  docs/
    report.md
    notes/                      one file per phase
    reviews/                    code reviews: findings, fixes and tests to add
```

**Tooling**

| Need | Tool |
| --- | --- |
| Redis client | go-redis v9 (`github.com/redis/go-redis/v9`) |
| Membership and gossip transport | memberlist (`github.com/hashicorp/memberlist`) |
| Metrics | Prometheus Go client (`github.com/prometheus/client_golang`), Grafana |
| Config parsing | `gopkg.in/yaml.v3`, or its maintained fork `go.yaml.in/yaml/v3` |
| Logging | `log/slog` from the standard library |
| Load generation | Own Go load generator (needed for the replay log); vegeta or k6 only as a cross-check |
| Fault injection | toxiproxy for Redis delay; `docker compose kill` and `stop` for crashes; the debug block endpoint for partitions |
| Integration tests | testcontainers-go with its Redis module |
| Cluster | Docker Compose on your laptop |
| Plots | Python with matplotlib, reading `summary.csv` |

Use the latest release of each library (`go get <module>@latest`); the import paths above are from memory, so confirm each one resolves before building on it.

**Risks**

- **Go is new to you.** Phase 0 exists for this; keep Phase 1 small.
- **Gossip scope creep.** Use memberlist first. Your contribution is the counter design and the measurements, not the membership protocol.
- **Noisy laptop benchmarks.** Pin CPU limits per container, repeat runs, report medians.
- **Placement season.** Phases 1 and 2 plus a small Phase 4 are a complete project. Treat gossip as the upgrade.

**Open questions**

- [ ] Which two measured numbers headline the resume bullet?
- [ ] Custom load generator from day one, or vegeta first and custom in Phase 4?
- [ ] Own SWIM implementation: worth it, or leave in the backlog?
