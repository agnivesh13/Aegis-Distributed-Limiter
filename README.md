# Aegis: a distributed rate-limiting API gateway

An HTTP gateway in Go that enforces per-client rate limits shared across several gateway instances, built to measure what exactness costs: Redis with Lua scripts (exact) against gossip between gateways (approximate).

> Work in progress. Results, charts and the write-up come in Phase 5. See [PLAN.md](PLAN.md) for the design and [TASKS.md](TASKS.md) for progress.

## Requirements

- Go (version in `go.mod`)
- Docker with Compose

## Build and test

```
go build ./...
go vet ./...
go test ./...                       # unit tests
go test -race ./...                 # needs cgo; on Windows run it in Docker (see TASKS.md)
go test -tags integration ./...     # adds the Redis tests; starts Redis in Docker
```

## Run

```
# one gateway, memory backend
docker compose -f deploy/docker-compose.yml up -d --build --wait upstream gateway
curl -i -H "X-API-Key: demo" http://localhost:8080/api/hello

# three gateways sharing one exact limit through Redis
LIMITER_BACKEND=redis docker compose -f deploy/docker-compose.yml up -d --build --wait --scale gateway=3

# send load: 200 requests per second for 30 s, one key, results to a CSV
docker compose -f deploy/docker-compose.yml run --rm loadgen -rate 200 -duration 30s -key k1 -out /results/raw/run.csv

# accuracy and latency of that run
go run ./cmd/analyze -in bench/results/raw/run.csv -scenario demo -backend redis -gateways 3

docker compose -f deploy/docker-compose.yml down
```

On PowerShell set the variable first: `$env:LIMITER_BACKEND = "redis"`.

| Port | What |
| --- | --- |
| 8080 to 8084 | Gateway replicas (proxy) |
| 9090 to 9094 | Gateway replicas (admin: `/healthz`, `/readyz`, `/metrics`, `/debug/limiter?key=`) |
| 8089 | Upstream echo service (port 8081 inside the Compose network) |
| 6379 | Redis |
| 9099, 3000 | Prometheus, Grafana (`--profile observability`) |

The observability stack is for local use only: Grafana allows anonymous viewing and its admin login is `admin`/`admin`. Do not publish these ports on a shared network.

## Layout

| Path | What |
| --- | --- |
| `cmd/gateway` | The gateway |
| `cmd/upstream` | Dummy echo service |
| `cmd/loadgen` | Open-loop load generator |
| `cmd/analyze` | Accuracy and latency from a run's CSV |
| `internal/limiter` | The limiter contract and its three backends: `memory`, `redis`, `gossip` |
| `internal/proxy` | Reverse proxy and the middleware chain |
| `internal/config` | YAML, environment overrides, validation |
| `learnings/` | Notes explaining every part from the basics |
