package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEchoesTheRequest(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/hello?x=1", strings.NewReader("12345"))
	req.Header.Set("X-Request-ID", "rid-1")
	rec := httptest.NewRecorder()
	handler(0).ServeHTTP(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"method": "POST", "path": "/api/hello", "query": "x=1", "body_bytes": float64(5), "request_id": "rid-1", "forwarded": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestDelay(t *testing.T) {
	start := time.Now()
	handler(30*time.Millisecond).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if took := time.Since(start); took < 30*time.Millisecond {
		t.Errorf("answered after %v, want at least 30ms", took)
	}
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(time.Hour).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("/healthz = %d", rec.Code)
	}
}
