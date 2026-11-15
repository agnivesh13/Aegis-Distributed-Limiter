package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/limitertest"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/memory"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/metrics"
)

var routes = []Route{
	{Name: "api", PathPrefix: "/api/", Cost: 1, Limit: limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 3, Period: time.Second}},
	{Name: "search", PathPrefix: "/api/search", Cost: 5, Limit: limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 10, Period: time.Second}},
}

// stubLimiter records what it is asked and answers what the test says.
type stubLimiter struct {
	mu    sync.Mutex
	keys  []string
	costs []int
	err   error
	panic bool
}

func (s *stubLimiter) Allow(_ context.Context, key string, l limiter.Limit, cost int) (limiter.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panic {
		panic("boom")
	}
	s.keys = append(s.keys, key)
	s.costs = append(s.costs, cost)
	if s.err != nil {
		return limiter.Decision{}, s.err
	}
	return limiter.Decision{Allowed: true, Limit: l.Rate, Remaining: 1}, nil
}

func (s *stubLimiter) Close() error { return nil }

type gateway struct {
	h        http.Handler
	m        *metrics.Metrics
	upstream *httptest.Server
	hits     *int
	mu       *sync.Mutex
	lastReq  *http.Request
}

func newGateway(t *testing.T, lim limiter.Limiter, configure func(*Options)) *gateway {
	t.Helper()
	g := &gateway{hits: new(int), mu: &sync.Mutex{}}
	g.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		*g.hits++
		g.lastReq = r.Clone(context.Background())
		g.mu.Unlock()
		w.Header().Set("X-Upstream", "yes")
		io.WriteString(w, "hello from "+r.URL.Path)
	}))
	t.Cleanup(g.upstream.Close)
	u, _ := url.Parse(g.upstream.URL)
	g.m = metrics.New()
	o := Options{
		GatewayID: "gw-test", Upstream: u, Routes: routes, Limiter: lim, Backend: "memory",
		Timeout: time.Second, Metrics: g.m, Logger: slog.New(slog.DiscardHandler),
	}
	if configure != nil {
		configure(&o)
	}
	g.h = New(o)
	return g
}

func (g *gateway) do(method, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "10.1.2.3:5555"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return rec
}

func (g *gateway) metricsText(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	g.m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestAllowedRequestIsProxiedWithHeaders(t *testing.T) {
	g := newGateway(t, &stubLimiter{}, nil)
	rec := g.do("GET", "/api/items?x=1", nil)

	if rec.Code != 200 || rec.Body.String() != "hello from /api/items" {
		t.Fatalf("got %d %q, want the upstream's answer", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	for name, want := range map[string]string{
		HeaderGatewayID: "gw-test", HeaderLimit: "3", HeaderRemaining: "1", HeaderReset: "0", "X-Upstream": "yes",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if len(h.Get(HeaderRequestID)) != 32 {
		t.Errorf("request ID %q, want 32 hex characters", h.Get(HeaderRequestID))
	}
	if got := g.lastReq.Header.Get(HeaderRequestID); got != h.Get(HeaderRequestID) {
		t.Errorf("upstream saw request ID %q, client was given %q", got, h.Get(HeaderRequestID))
	}
	if got := g.lastReq.Header.Get("X-Forwarded-For"); got != "10.1.2.3" {
		t.Errorf("upstream saw X-Forwarded-For %q, want the client IP", got)
	}
	if got := g.lastReq.URL.RawQuery; got != "x=1" {
		t.Errorf("upstream saw query %q, want x=1", got)
	}
}

func TestClientRequestIDIsKept(t *testing.T) {
	g := newGateway(t, &stubLimiter{}, nil)
	rec := g.do("GET", "/api/x", map[string]string{HeaderRequestID: "abc-123"})
	if got := rec.Header().Get(HeaderRequestID); got != "abc-123" {
		t.Errorf("request ID = %q, want the client's abc-123", got)
	}
	rec = g.do("GET", "/api/x", map[string]string{HeaderRequestID: "bad id\x01"})
	if got := rec.Header().Get(HeaderRequestID); got == "bad id\x01" || len(got) != 32 {
		t.Errorf("request ID = %q, want an unsafe one replaced", got)
	}
}

func TestLongestPrefixWinsAndSetsKeyAndCost(t *testing.T) {
	lim := &stubLimiter{}
	g := newGateway(t, lim, nil)
	g.do("GET", "/api/search?q=go", map[string]string{HeaderAPIKey: "key-123"})
	g.do("GET", "/api/items", nil)

	want := []string{"search:key-123", "api:10.1.2.3"}
	if strings.Join(lim.keys, ",") != strings.Join(want, ",") {
		t.Errorf("limiter keys = %v, want %v", lim.keys, want)
	}
	if lim.costs[0] != 5 || lim.costs[1] != 1 {
		t.Errorf("costs = %v, want [5 1]", lim.costs)
	}
}

func TestNoRouteIs404AndNeverAsksTheLimiter(t *testing.T) {
	lim := &stubLimiter{}
	g := newGateway(t, lim, nil)
	rec := g.do("GET", "/other", nil)
	if rec.Code != 404 || rec.Header().Get(HeaderGatewayID) != "gw-test" {
		t.Errorf("got %d with gateway ID %q, want 404 with the ID", rec.Code, rec.Header().Get(HeaderGatewayID))
	}
	if len(lim.keys) != 0 || *g.hits != 0 {
		t.Errorf("limiter asked %d times, upstream hit %d times; want neither", len(lim.keys), *g.hits)
	}
}

// With a real limiter: allowed until the limit, then 429 with headers.
func TestRejectedRequestGets429(t *testing.T) {
	clock := limitertest.NewFakeClock(time.Unix(1_700_000_000, 250_000_000))
	lim := memory.New(clock)
	defer lim.Close()
	g := newGateway(t, lim, nil)

	for i := range 3 {
		if rec := g.do("GET", "/api/x", nil); rec.Code != 200 {
			t.Fatalf("request %d: %d, want 200", i+1, rec.Code)
		}
	}
	rec := g.do("GET", "/api/x", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 4: %d, want 429", rec.Code)
	}
	h := rec.Header()
	// 750 ms to the end of the window, rounded up to whole seconds.
	for name, want := range map[string]string{HeaderRetry: "1", HeaderLimit: "3", HeaderRemaining: "0", HeaderReset: "2", HeaderGatewayID: "gw-test"} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["retry_after_seconds"] != float64(1) {
		t.Errorf("body %q, want JSON with retry_after_seconds 1", rec.Body.String())
	}
	if *g.hits != 3 {
		t.Errorf("upstream hit %d times, want 3", *g.hits)
	}

	text := g.metricsText(t)
	for _, want := range []string{
		`gateway_requests_total{decision="allowed",route="api"} 3`,
		`gateway_requests_total{decision="rejected",route="api"} 1`,
		`gateway_limiter_decision_seconds_count{algorithm="sliding_window",backend="memory"} 4`,
		`gateway_upstream_seconds_count{route="api"} 3`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestLimiterErrorFailOpen(t *testing.T) {
	g := newGateway(t, &stubLimiter{err: errors.New("redis down")}, nil)
	rec := g.do("GET", "/api/x", nil)
	if rec.Code != 200 || *g.hits != 1 {
		t.Errorf("got %d, upstream hit %d; want the request proxied", rec.Code, *g.hits)
	}
	if rec.Header().Get(HeaderLimit) != "" {
		t.Error("rate-limit headers set although the limiter did not decide")
	}
	text := g.metricsText(t)
	for _, want := range []string{`gateway_limiter_errors_total{backend="memory"} 1`, `gateway_requests_total{decision="error",route="api"} 1`} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestLimiterErrorFailClosed(t *testing.T) {
	g := newGateway(t, &stubLimiter{err: errors.New("redis down")}, func(o *Options) { o.FailClosed = true })
	rec := g.do("GET", "/api/x", nil)
	if rec.Code != http.StatusServiceUnavailable || *g.hits != 0 {
		t.Errorf("got %d, upstream hit %d; want 503 and nothing proxied", rec.Code, *g.hits)
	}
}

// The limiter's deadline is applied to every call.
func TestLimiterGetsADeadline(t *testing.T) {
	var deadline time.Duration
	lim := limiterFunc(func(ctx context.Context) {
		if d, ok := ctx.Deadline(); ok {
			deadline = time.Until(d)
		}
	})
	g := newGateway(t, lim, func(o *Options) { o.Timeout = 50 * time.Millisecond })
	g.do("GET", "/api/x", nil)
	if deadline <= 0 || deadline > 50*time.Millisecond {
		t.Errorf("limiter saw a deadline %v away, want at most 50ms", deadline)
	}
}

type limiterFunc func(ctx context.Context)

func (f limiterFunc) Allow(ctx context.Context, _ string, l limiter.Limit, _ int) (limiter.Decision, error) {
	f(ctx)
	return limiter.Decision{Allowed: true, Limit: l.Rate}, nil
}
func (f limiterFunc) Close() error { return nil }

func TestUpstreamDownIs502(t *testing.T) {
	g := newGateway(t, &stubLimiter{}, nil)
	g.upstream.Close()
	if rec := g.do("GET", "/api/x", nil); rec.Code != http.StatusBadGateway {
		t.Errorf("got %d, want 502", rec.Code)
	}
}

func TestPanicBecomes500(t *testing.T) {
	g := newGateway(t, &stubLimiter{panic: true}, nil)
	rec := g.do("GET", "/api/x", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", rec.Code)
	}
}

func TestClientID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header map[string]string
		remote string
		trust  bool
		want   string
	}{
		{"api key wins", map[string]string{HeaderAPIKey: " k1 ", "X-Forwarded-For": "9.9.9.9"}, "1.1.1.1:1", true, "k1"},
		{"remote address", nil, "1.1.1.1:1234", false, "1.1.1.1"},
		{"ipv6 remote address", nil, "[2001:db8::1]:1234", false, "2001:db8::1"},
		{"forwarded-for ignored unless trusted", map[string]string{"X-Forwarded-For": "9.9.9.9"}, "1.1.1.1:1", false, "1.1.1.1"},
		{"forwarded-for first hop when trusted", map[string]string{"X-Forwarded-For": "9.9.9.9, 8.8.8.8"}, "1.1.1.1:1", true, "9.9.9.9"},
		{"remote without port", nil, "1.1.1.1", false, "1.1.1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			if got := ClientID(r, tc.trust); got != tc.want {
				t.Errorf("ClientID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCeilSeconds(t *testing.T) {
	for d, want := range map[time.Duration]int{0: 0, -time.Second: 0, time.Nanosecond: 1, 999 * time.Millisecond: 1, time.Second: 1, 1001 * time.Millisecond: 2} {
		if got := ceilSeconds(d); got != want {
			t.Errorf("ceilSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}
