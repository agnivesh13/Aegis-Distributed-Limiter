# 11. The gateway: config, the middleware chain, metrics and shutdown

Files: [internal/config/](../internal/config/), [internal/proxy/](../internal/proxy/), [internal/metrics/](../internal/metrics/), [internal/admin/](../internal/admin/), [cmd/gateway/main.go](../cmd/gateway/main.go)

Notes 03 to 10 built limiters. This note is about the program that uses one: a web server that sits in front of another service, checks every request with the limiter, and either forwards it or turns it away.

## What a reverse proxy is

A **proxy** passes messages along on someone's behalf. A **reverse proxy** stands in front of a server: clients talk to the proxy, and the proxy talks to the real server (the **upstream**) for them.

```text
client  ──►  gateway :8080  ──►  upstream :8081
        ◄──                 ◄──
```

The client never sees the upstream. That gives the gateway one place to do things every request needs, such as rate limiting, before the upstream does any work.

Go ships a reverse proxy in its standard library, `httputil.ReverseProxy`. We only configure it:

```go
rp := &httputil.ReverseProxy{
	Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(o.Upstream)   // send it to the upstream
		pr.SetXForwarded()      // tell the upstream who the real client was
	},
	ErrorHandler: ...           // upstream unreachable → 502
}
```

`SetXForwarded` adds an `X-Forwarded-For` header. Without it the upstream would think every request came from the gateway.

## Configuration

### Three layers

The gateway's settings come from three places, each overriding the one before:

1. **Defaults** in code (`config.Default()`): listen on `:8080`, memory backend, and so on.
2. **The YAML file** ([deploy/gateway.yaml](../deploy/gateway.yaml)).
3. **Environment variables**, such as `LIMITER_BACKEND=redis`.

Why environment variables at all? A container image is built once and run many ways. The bench runner needs to start the same image as "Redis, 3 gateways" and then as "gossip, 10 ms interval" without editing a file in between. Changing an environment variable is the standard way to do that.

### Strict parsing

```go
dec.KnownFields(true) // a misspelt key is an error, not a silent default
```

Without this line, a file saying `limitter: { backend: redis }` would parse without complaint. The misspelt section would be ignored, the gateway would quietly use the memory backend, and the experiment would measure the wrong thing. Failing loudly at start-up is far better than running wrongly.

### Report every problem at once

```go
var errs []error
bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
...
return errors.Join(errs...)
```

Validation collects every problem and returns them together. Fixing a config one error per restart is tedious; seeing all eleven at once is not. `errors.Join` (Go 1.20 and later) combines many errors into one.

Some checks worth noticing:

- The route name may not contain `:`, because the limiter key is `route:client`. A route called `a:b` would make keys ambiguous.
- `cost` must not exceed the limit's capacity. Such a request could never pass, so the config is wrong.
- The gossip backend refuses `token_bucket` routes, as the backend matrix says.
- Redis and gossip need periods in whole milliseconds; only memory accepts `1500us`.

## The middleware chain

### What middleware is

A **middleware** is a function that takes a handler and returns a new handler that does something extra before or after calling the original:

```go
func requestID(gatewayID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// before: give the request an ID
		next.ServeHTTP(w, r)
		// after: (nothing here)
	})
}
```

Wrapping one inside another builds a chain:

```go
var h http.Handler = upstream
h = rateLimit(o, h)
h = clientIdentity(o.TrustForwardedFor, h)
h = routeMatch(routes, h)
h = recordMetrics(o.Metrics, h)
h = requestID(o.GatewayID, h)
h = recoverPanics(o.Logger, h)
```

Read it bottom to top for the order a request travels: recover, request ID, metrics, route match, client identity, rate limit, then the upstream. Each layer is small and testable alone.

### Why this order

| Layer | Why it sits where it does |
| --- | --- |
| recover | Outermost, so a panic anywhere below is caught |
| request ID | Early, so even a 404 or a panic log line has an ID |
| metrics | Wraps everything after it, so it sees the final decision |
| route match | The limiter needs to know which limit applies |
| client identity | The limiter needs to know whose count to charge |
| rate limit | Last before the upstream: everything it needs is known |

### Passing information down the chain

The metrics layer runs *before* the rate limiter but must record the limiter's decision. How does information flow back up?

Through a small struct shared by pointer in the request's **context**:

```go
info := &requestInfo{}
next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
if info.route != nil && info.decision != "" {
	m.Requests.WithLabelValues(info.route.Name, info.decision).Inc()
}
```

Each later layer fills in its part (`info.route`, `info.client`, `info.decision`). When the chain returns, the metrics layer reads the filled-in struct. The context is Go's standard way to carry request-scoped values through layers that do not know about each other.

### Who is the client?

```go
func ClientID(r *http.Request, trustForwardedFor bool) string {
	if key := r.Header.Get("X-API-Key"); key != "" { return key }
	if trustForwardedFor { ...first entry of X-Forwarded-For... }
	return host part of r.RemoteAddr
}
```

The `X-Forwarded-For` rule matters for security. **Any client can write any header.** If the gateway always believed `X-Forwarded-For`, a client could put a different made-up address in every request and never be limited. It should be trusted only when the gateway sits behind a load balancer you control, which writes the header itself. Hence the switch, off by default.

### The rate-limit layer

```go
ctx, cancel := context.WithTimeout(r.Context(), o.Timeout)
d, err := o.Limiter.Allow(ctx, route.Name+":"+info.client, route.Limit, route.Cost)
cancel()
```

Three outcomes:

| Outcome | Response |
| --- | --- |
| Allowed | Forward, adding `X-RateLimit-Limit`, `-Remaining`, `-Reset` |
| Rejected | `429` with the same headers plus `Retry-After`, and a short JSON body |
| Limiter error | `fail_open`: forward anyway. `fail_closed`: `503` |

`Retry-After` must be whole seconds. Rounding *down* would tell a client to wait 0 seconds when the real wait is 300 ms, and it would come straight back. So the gateway always rounds up (`ceilSeconds`), and never sends less than 1 on a rejection.

### Catching panics

```go
defer func() {
	v := recover()
	if v == nil { return }
	if v == http.ErrAbortHandler { panic(v) }
	log.Error(...)
	writeJSON(w, 500, ...)
}()
```

A **panic** is Go's crash. Inside a `defer`, `recover()` catches it, so one bad request becomes a 500 and a log line instead of stopping the server.

One panic is let through on purpose: `http.ErrAbortHandler`. The reverse proxy uses it to abandon a response half-way, for example when the upstream dies mid-stream, and net/http expects to see it.

## Metrics

Prometheus works by **pulling**: every few seconds it asks each gateway for `/metrics` and stores the numbers. Three kinds are used:

| Kind | What it holds | Example |
| --- | --- | --- |
| Counter | A number that only goes up | `gateway_requests_total` |
| Gauge | A number that goes up and down | `gossip_members` |
| Histogram | Counts of observations in ranges ("buckets") | `gateway_limiter_decision_seconds` |

**Labels** split one metric into series: `gateway_requests_total{route="api",decision="rejected"}`. Labels must have a small, fixed set of values. Putting the client's API key in a label would create a new series per client and swamp Prometheus. That is why the client is never a label.

### Histograms and buckets

You cannot store every latency. A histogram stores how many observations fell at or below each **bucket boundary**. The plan fixes the decision buckets: start at 10 µs, multiply by 1.5 each time, 24 buckets, reaching about 110 ms. Multiplying (not adding) gives fine detail at small values and coarse detail at large ones, which suits latencies.

To estimate a percentile, find the bucket where the running count passes the target and interpolate inside it. The answer is only as precise as the bucket. In S0 the memory backend's p99 came out as 9.9 µs. That just means "below the first bucket": the memory backend decides faster than this histogram can tell apart.

### A private registry

```go
reg := prometheus.NewRegistry()
```

The Prometheus library has a global default registry, but registering the same metric twice there panics. Each test creates a gateway, so tests would collide. A registry per `Metrics` object keeps them independent.

### Values read on scrape

The gossip counters live inside the gossip node as plain atomic numbers. Instead of copying them into Prometheus counters all the time, the gateway registers a function that reads them only when Prometheus asks:

```go
prometheus.NewCounterFunc(..., func() float64 { return float64(stats().PacketsSent) })
```

## The admin port

The gateway listens on two ports: `:8080` for traffic and `:9090` for operators. Keeping them apart means the admin endpoints are never rate limited and need never be exposed to the internet.

**Health and readiness are different questions:**

| Endpoint | Question | Redis down |
| --- | --- | --- |
| `/healthz` | Is the process alive? | 200: restarting would not help |
| `/readyz` | Should traffic come here? | 503: send it elsewhere |

Mixing them up causes real outages: if `/healthz` checked Redis, an orchestrator would restart every gateway the moment Redis blinked, which fixes nothing.

## Wiring and shutdown: `cmd/gateway`

`main` is short on purpose. It calls `run`, which returns an error; `main` logs it and exits with status 1. Code that returns errors is easier to read and test than code that calls `os.Exit` from deep inside.

### Graceful shutdown

When Docker stops a container it sends `SIGTERM`, waits, then kills it. In between, the gateway should finish what it started:

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
...
<-ctx.Done()                          // a signal arrived
draining.Store(true)                  // 1. /readyz now says 503
proxySrv.Shutdown(shutdownCtx)        // 2. stop accepting; wait for in-flight requests (up to 10 s)
adminSrv.Shutdown(shutdownCtx)
b.limiter.Close()                     // 3. only now close the limiter
```

The order matters. Closing the limiter first would make in-flight requests fail. And reporting not-ready first gives anything that routes traffic a chance to stop sending before the door closes.

### A start-up check I added

The first time three gateways ran on Redis, every request was let through. Nothing was broken: the route used the sliding window, the Redis sliding-window script is still a stub, every `Allow` returned "unsupported", and `fail_open` did exactly its job. But nothing *said* so, apart from an error counter nobody was looking at.

So the gateway now asks its backend once per route at start-up, using a key no client can send (it contains a NUL byte, which HTTP headers cannot):

```go
_, err := lim.Allow(probeCtx, "\x00startup-probe:"+r.Name, r.Limit, 1)
if errors.Is(err, limiter.ErrUnsupported) {
	log.Error("this backend cannot decide this route's algorithm yet; every request on the route will follow on_error", ...)
}
```

It logs rather than refusing to start, because `fail_open` is a legitimate choice and your token bucket is still being written. But the problem is now the first thing in the log.

## Check yourself

- What is a reverse proxy, and why put rate limiting in one?
- In what order do the three configuration layers apply, and why are environment variables useful for experiments?
- What would `KnownFields(true)` have saved you from?
- Read the middleware chain in the code and give the order a request travels. Why is `recover` outermost?
- How does the metrics layer learn a decision that is made after it runs?
- Why is `X-Forwarded-For` off by default?
- Why must `Retry-After` round up?
- Why is the API key never a Prometheus label?
- What is the difference between `/healthz` and `/readyz`, and what goes wrong if you mix them up?
- Give the three shutdown steps in order and say why the order matters.
