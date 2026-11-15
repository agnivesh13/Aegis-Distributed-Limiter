// Command upstream is the dummy service behind the gateway. It answers
// every request with a small JSON description of what it received, after
// an optional delay.
//
//	upstream -addr :8081 -delay 2ms
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8081", "address to listen on")
	delay := flag.Duration("delay", 0, "artificial delay before every answer")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv := &http.Server{Addr: *addr, Handler: handler(*delay), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("upstream started", "addr", *addr, "delay", delay.String())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("upstream stopped", "err", err)
		os.Exit(1)
	}
}

func handler(delay time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`+"\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		n, _ := io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"method":     r.Method,
			"path":       r.URL.Path,
			"query":      r.URL.RawQuery,
			"body_bytes": n,
			"request_id": r.Header.Get("X-Request-ID"),
			"forwarded":  r.Header.Get("X-Forwarded-For"),
		})
	})
	return mux
}
