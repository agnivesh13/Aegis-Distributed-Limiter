package middleware

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// A table-driven test: the cases are data, and one loop runs them all.
// Adding a case is adding a line.
func TestLogging(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantLog    string
	}{
		{"ok", "GET", "/hello", 200, "status=200"},
		{"handler sets a status", "GET", "/teapot", 418, "status=418"},
		{"no such path", "GET", "/missing", 404, "status=404"},
		{"wrong method", "POST", "/hello", 405, "status=405"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := Logging(slog.New(slog.NewTextHandler(&logs, nil)), Hello())

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			line := logs.String()
			for _, want := range []string{tc.wantLog, "method=" + tc.method, "path=" + tc.path, "took="} {
				if !strings.Contains(line, want) {
					t.Errorf("log line %q is missing %q", strings.TrimSpace(line), want)
				}
			}
			if strings.Count(line, "\n") != 1 {
				t.Errorf("logged %d lines for one request, want 1", strings.Count(line, "\n"))
			}
		})
	}
}
