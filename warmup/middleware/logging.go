// Package middleware is warm-up exercise 4: an HTTP handler wrapped in one
// logging middleware. The chain in internal/proxy is seven of these, one
// inside the next.
package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder remembers the status code a handler writes. An
// http.ResponseWriter does not let you read the status back, so the
// middleware wraps it and notes the code on the way through.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Logging logs one line per request: method, path, status and how long it
// took. Code before next.ServeHTTP runs on the way in; code after it runs
// on the way out.
func Logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK} // 200 unless the handler says otherwise
		next.ServeHTTP(rec, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start))
	})
}

// Hello is the handler being wrapped.
func Hello() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello\n"))
	})
	mux.HandleFunc("GET /teapot", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "short and stout", http.StatusTeapot)
	})
	return mux
}
