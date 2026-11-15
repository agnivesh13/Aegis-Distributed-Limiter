package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHealthAndReadiness(t *testing.T) {
	var readyErr error
	h := Handler(Options{GatewayID: "gw-1", Backend: "redis", Ready: func(context.Context) error { return readyErr }})

	if rec := call(h, "GET", "/healthz"); rec.Code != 200 {
		t.Errorf("/healthz = %d", rec.Code)
	}
	if rec := call(h, "GET", "/readyz"); rec.Code != 200 {
		t.Errorf("/readyz while ready = %d", rec.Code)
	}
	readyErr = errors.New("redis unreachable")
	rec := call(h, "GET", "/readyz")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "redis unreachable") {
		t.Errorf("/readyz while not ready = %d %s", rec.Code, rec.Body.String())
	}
	// Health is about the process, not its dependencies: still fine.
	if rec := call(h, "GET", "/healthz"); rec.Code != 200 {
		t.Errorf("/healthz while not ready = %d, want 200", rec.Code)
	}
}

func TestDebugLimiter(t *testing.T) {
	off := Handler(Options{Backend: "memory"})
	if rec := call(off, "GET", "/debug/limiter?key=api:k"); rec.Code != 404 {
		t.Errorf("without a debug view: %d, want 404", rec.Code)
	}

	on := Handler(Options{GatewayID: "gw-1", LimiterDebug: func(key string) any { return map[string]int{"total": 7} }})
	if rec := call(on, "GET", "/debug/limiter"); rec.Code != 400 {
		t.Errorf("missing key: %d, want 400", rec.Code)
	}
	rec := call(on, "GET", "/debug/limiter?key=api:k")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"total":7`) || !strings.Contains(rec.Body.String(), `"key":"api:k"`) {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestBlockEndpoint(t *testing.T) {
	off := Handler(Options{})
	if rec := call(off, "POST", "/debug/gossip/block?peer=gw-2"); rec.Code != 404 {
		t.Errorf("faults off: %d, want 404", rec.Code)
	}

	blocked := map[string]bool{}
	on := Handler(Options{Block: func(peer string, b bool) { blocked[peer] = b }})
	if rec := call(on, "POST", "/debug/gossip/block?peer=gw-2"); rec.Code != 200 || !blocked["gw-2"] {
		t.Errorf("block: %d, state %v", rec.Code, blocked)
	}
	if rec := call(on, "DELETE", "/debug/gossip/block?peer=gw-2"); rec.Code != 200 || blocked["gw-2"] {
		t.Errorf("unblock: %d, state %v", rec.Code, blocked)
	}
	if rec := call(on, "POST", "/debug/gossip/block"); rec.Code != 400 {
		t.Errorf("missing peer: %d, want 400", rec.Code)
	}
	if rec := call(on, "GET", "/debug/gossip/block?peer=gw-2"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", rec.Code)
	}
}
