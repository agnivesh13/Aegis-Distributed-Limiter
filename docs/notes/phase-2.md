# Phase 2: Redis and Lua, exact and shared

Any number of gateways enforce one exact shared limit. The long explanations are in [learnings/](../../learnings/README.md), notes 08, 12 and 16.

## What was built

| Piece | Where | By |
| --- | --- | --- |
| Token bucket and sliding window scripts | `internal/limiter/redis/scripts` | Fable |
| Redis backend: embedded scripts, deadline, reply mapping, `Ping` | `internal/limiter/redis` | Fable |
| Integration tests and a naive GET-then-SET backend | `internal/limiter/redis/integration_test.go` | Fable |
| `fail_open` and `fail_closed`, `/readyz` checking Redis | `internal/proxy`, `internal/admin`, `cmd/gateway` | Opus |
| Compose: Redis, scalable gateway, toxiproxy; load generator fan-out over replicas | `deploy/`, `cmd/loadgen` | Opus |

## Design choices

- **Each decision is one Lua script.** Redis runs a script as one command, so read, decide and write cannot interleave with another gateway's.
- **The scripts read the time from Redis** (`TIME`), so gateway clocks cannot disagree. A test-only argument injects the time for the conformance suite.
- **Scripts return whole milliseconds, rounded up.** Redis truncates any number a script returns.
- **Retries are off.** If a script ran and its reply was lost, a retry would charge the request twice.
- **The per-call deadline is enforced on socket reads** (`ContextTimeoutEnabled`); by default the client library would ignore it.
- **Keys expire in Redis** (`PEXPIRE`), which replaces the memory backend's janitor.
- **Integration tests are behind the `integration` build tag** and use a real Redis, started by testcontainers or given through `REDIS_ADDR`.
- **Outages are simulated with a small TCP proxy inside the test,** so Redis can be taken away and brought back on the same address, and can be made to accept connections and never answer.
- **On start-up the gateway logs an error for any route its backend cannot decide,** after a run in which a missing script failed open on every request without a sign.

## What each test proves

| Test | Proves |
| --- | --- |
| `TestConformance` (integration) | The Redis backend passes the same 29 cases as memory, against real Redis |
| `TestThreeInstancesAdmitExactlyTheLimit` | Three backends sharing one Redis admit exactly the limit under 10,000 concurrent requests, for both algorithms |
| `TestNaiveBackendAdmitsMoreThanTheLimit` | GET, decide in Go, SET over-admits: 2,032 to 2,336 against a limit of 100 |
| `TestRedisOutageAndRecovery` | Refused connections fail fast; a silent connection is cut at the deadline; the same backend recovers unaided |
| `TestAllowRejectsBadInputWithoutCallingRedis` | Bad algorithm, cost or period is settled before any network call |

## Done when

| Check | Result |
| --- | --- |
| `go test -race -tags integration ./...` | Passes |
| 3 gateways, 200 req/s against 100 req/s, 30 s, inside the 2% band | 3,000 admitted; ideal 3,000 |
| Redis stopped under load | `fail_open`: 1,670 requests proxied, error counter up. `fail_closed`: 1,664 answered 503. Both recovered unaided; client p95 stayed at the 50 ms deadline |

Results: `bench/results/phase2-naive-race.txt`, `bench/results/phase2-redis-cluster.txt`.
