// Package proxy is the gateway's request path: a reverse proxy behind a
// chain of middleware that ends in the rate limiter.
//
// The chain runs in the order the plan fixes:
//
//	recover → request ID → metrics → route match → client identity → rate limit → proxy
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/metrics"
)

// Route is one entry of the routing table.
type Route struct {
	Name       string
	PathPrefix string
	Cost       int
	Limit      limiter.Limit
}

// Options configures the handler.
type Options struct {
	GatewayID         string
	Upstream          *url.URL
	Routes            []Route
	Limiter           limiter.Limiter
	Backend           string        // backend name, for metric labels
	FailClosed        bool          // on a limiter error, answer 503 instead of proxying
	Timeout           time.Duration // deadline for one Allow call
	TrustForwardedFor bool
	Metrics           *metrics.Metrics
	Logger            *slog.Logger

	// Transport sends requests upstream. Nil means a transport tuned for
	// many concurrent requests to one upstream.
	Transport http.RoundTripper
}

// Response headers the gateway sets.
const (
	HeaderGatewayID = "X-Gateway-ID"
	HeaderRequestID = "X-Request-ID"
	HeaderAPIKey    = "X-API-Key"
	HeaderLimit     = "X-RateLimit-Limit"
	HeaderRemaining = "X-RateLimit-Remaining"
	HeaderReset     = "X-RateLimit-Reset"
	HeaderRetry     = "Retry-After"
)

// requestInfo travels down the chain in the request's context. Each
// middleware fills in what it learns; the metrics middleware reads it once
// the request is done.
type requestInfo struct {
	route    *Route
	client   string
	decision string // metrics.Allowed, Rejected or Error; empty if never decided
}

type infoKey struct{}

func infoFrom(r *http.Request) *requestInfo {
	info, _ := r.Context().Value(infoKey{}).(*requestInfo)
	return info
}

// New returns the gateway's main handler.
func New(o Options) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Transport == nil {
		o.Transport = DefaultTransport()
	}

	// Longest prefix first, so the first match is the longest match.
	routes := append([]Route(nil), o.Routes...)
	sort.SliceStable(routes, func(i, j int) bool { return len(routes[i].PathPrefix) > len(routes[j].PathPrefix) })

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(o.Upstream)
			pr.SetXForwarded()
		},
		Transport: o.Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				o.Logger.Warn("upstream request failed", "request_id", r.Header.Get(HeaderRequestID), "err", err)
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream unreachable"})
		},
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rp.ServeHTTP(w, r)
		if info := infoFrom(r); info != nil && info.route != nil {
			o.Metrics.Upstream.WithLabelValues(info.route.Name).Observe(time.Since(start).Seconds())
		}
	})

	var h http.Handler = upstream
	h = rateLimit(o, h)
	h = clientIdentity(o.TrustForwardedFor, h)
	h = routeMatch(routes, h)
	h = recordMetrics(o.Metrics, h)
	h = requestID(o.GatewayID, h)
	h = recoverPanics(o.Logger, h)
	return h
}

// DefaultTransport keeps plenty of idle connections to the one upstream, so
// a load test does not spend its time opening TCP connections.
func DefaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 1024
	t.MaxIdleConnsPerHost = 1024
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// recoverPanics turns a panic in any later handler into a 500 and a log
// line, so one bad request cannot take the gateway down.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				// The reverse proxy's way of abandoning a response
				// half-way through. net/http expects to see it.
				panic(v)
			}
			log.Error("panic while handling a request", "panic", v, "path", r.URL.Path, "request_id", r.Header.Get(HeaderRequestID))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		}()
		next.ServeHTTP(w, r)
	})
}

// requestID gives every request an ID, keeping a sane one the client sent,
// and stamps every response with the gateway's ID.
func requestID(gatewayID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			id = newRequestID()
			r.Header.Set(HeaderRequestID, id) // forwarded upstream with the request
		}
		w.Header().Set(HeaderRequestID, id)
		w.Header().Set(HeaderGatewayID, gatewayID)
		next.ServeHTTP(w, r)
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// recordMetrics counts each routed request by its decision once the rest of
// the chain has finished with it.
func recordMetrics(m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &requestInfo{}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
		if info.route != nil && info.decision != "" {
			m.Requests.WithLabelValues(info.route.Name, info.decision).Inc()
		}
	})
}

// routeMatch picks the route with the longest matching path prefix.
func routeMatch(routes []Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := range routes {
			if strings.HasPrefix(r.URL.Path, routes[i].PathPrefix) {
				infoFrom(r).route = &routes[i]
				next.ServeHTTP(w, r)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no route matches " + r.URL.Path})
	})
}

// MaxAPIKeyLen is the longest X-API-Key accepted. The key ends up in the
// limiter's key, so in memory, in Redis key names and in gossip packets:
// anything stored from outside needs a size limit.
const MaxAPIKeyLen = 128

// clientIdentity works out who is asking: the API key if there is one,
// else the client's IP address.
func clientIdentity(trustForwardedFor bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Get(HeaderAPIKey)) > MaxAPIKeyLen {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("X-API-Key is longer than %d bytes", MaxAPIKeyLen),
			})
			return
		}
		infoFrom(r).client = ClientID(r, trustForwardedFor)
		next.ServeHTTP(w, r)
	})
}

// ClientID returns the X-API-Key header, or failing that the client's IP.
//
// X-Forwarded-For is believed only when trustForwardedFor is set, because
// any client can write that header. Even then only its last entry is used:
// each proxy appends the address it saw, so the last entry was written by
// the trusted load balancer in front of the gateway, while everything
// before it came from the client. This assumes exactly one trusted proxy;
// behind several, count back that many entries instead.
func ClientID(r *http.Request, trustForwardedFor bool) string {
	if key := strings.TrimSpace(r.Header.Get(HeaderAPIKey)); key != "" {
		return key
	}
	if trustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			last := xff[strings.LastIndexByte(xff, ',')+1:]
			if last = strings.TrimSpace(last); last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimit asks the limiter about the request and answers 429 or 503, or
// lets it through to the upstream.
func rateLimit(o Options, next http.Handler) http.Handler {
	failure := metrics.Error
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := infoFrom(r)
		route := info.route

		ctx, cancel := context.WithTimeout(r.Context(), o.Timeout)
		start := time.Now()
		d, err := o.Limiter.Allow(ctx, route.Name+":"+info.client, route.Limit, route.Cost)
		cancel()
		o.Metrics.ObserveDecision(o.Backend, string(route.Limit.Algorithm), time.Since(start))

		if err != nil && r.Context().Err() != nil {
			// The client went away while the limiter was being asked, so
			// the call was cancelled. That says nothing about the
			// backend's health: count nothing and answer no one.
			return
		}
		if err != nil {
			info.decision = failure
			o.Metrics.LimiterErrors.WithLabelValues(o.Backend).Inc()
			o.Logger.Warn("limiter could not decide", "route", route.Name, "err", err, "fail_closed", o.FailClosed)
			if o.FailClosed {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "rate limiter unavailable"})
				return
			}
			next.ServeHTTP(w, r) // fail open: let it through, unlimited
			return
		}

		h := w.Header()
		h.Set(HeaderLimit, strconv.Itoa(d.Limit))
		h.Set(HeaderRemaining, strconv.Itoa(d.Remaining))
		h.Set(HeaderReset, strconv.Itoa(ceilSeconds(d.ResetAfter)))
		if !d.Allowed {
			info.decision = metrics.Rejected
			retry := max(1, ceilSeconds(d.RetryAfter))
			h.Set(HeaderRetry, strconv.Itoa(retry))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":               "rate limit exceeded",
				"retry_after_seconds": retry,
			})
			return
		}
		info.decision = metrics.Allowed
		next.ServeHTTP(w, r)
	})
}

// ceilSeconds rounds a duration up to whole seconds, as Retry-After needs:
// a client told to wait 0 seconds after a 300 ms wait would come back early.
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
