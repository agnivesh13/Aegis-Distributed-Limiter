// Package admin serves the gateway's admin port: health, readiness,
// metrics and debug endpoints. Nothing here is rate limited.
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Options says what the admin port can show. Nil functions switch their
// endpoint off (it answers 404).
type Options struct {
	GatewayID string
	Backend   string

	// Ready reports whether the gateway should receive traffic. It is
	// given a short deadline.
	Ready func(ctx context.Context) error

	// Metrics serves /metrics.
	Metrics http.Handler

	// LimiterDebug returns what the backend knows about one key, for
	// /debug/limiter?key=... Only the gossip backend has this.
	LimiterDebug func(key string) any

	// Block cuts this gateway off from a peer (block true) or reconnects
	// it (block false), for partition experiments. Set only when
	// DEBUG_FAULTS=1.
	Block func(peer string, block bool)
}

// Handler returns the admin port's mux.
func Handler(o Options) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "gateway_id": o.GatewayID})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if o.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if err := o.Ready(ctx); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not ready", "reason": err.Error(), "gateway_id": o.GatewayID})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "gateway_id": o.GatewayID, "backend": o.Backend})
	})

	if o.Metrics != nil {
		mux.Handle("GET /metrics", o.Metrics)
	}

	mux.HandleFunc("GET /debug/limiter", func(w http.ResponseWriter, r *http.Request) {
		if o.LimiterDebug == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "the " + o.Backend + " backend has no per-key debug view"})
			return
		}
		key := r.URL.Query().Get("key")
		if key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing ?key=route:client"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"gateway_id": o.GatewayID, "key": key, "state": o.LimiterDebug(key)})
	})

	blockHandler := func(block bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if o.Block == nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "fault injection is off; start the gateway with DEBUG_FAULTS=1 and the gossip backend"})
				return
			}
			peer := r.URL.Query().Get("peer")
			if peer == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing ?peer=<gateway id>"})
				return
			}
			o.Block(peer, block)
			writeJSON(w, http.StatusOK, map[string]any{"gateway_id": o.GatewayID, "peer": peer, "blocked": block})
		}
	}
	// POST blocks, DELETE unblocks: the same resource, switched on or off.
	mux.HandleFunc("POST /debug/gossip/block", blockHandler(true))
	mux.HandleFunc("DELETE /debug/gossip/block", blockHandler(false))

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
