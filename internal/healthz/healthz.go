// Package healthz exposes GET /api/health (200/401/503 convention) and GET /metrics.
package healthz

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Health wires the DB ping + catalog loaded checks.
type Health struct {
	PingFunc      func(ctx context.Context) error
	CatalogLoaded func() bool
}

// Routes returns a mux with /api/health and /metrics mounted.
func (h *Health) Routes(adminToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", h.handle(adminToken))
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

// HandleHealth returns the /api/health handler alone.
func (h *Health) HandleHealth(adminToken string) http.HandlerFunc { return h.handle(adminToken) }

// Metrics returns the Prometheus handler.
func Metrics() http.Handler { return promhttp.Handler() }

func (h *Health) handle(adminToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// When ADMIN_TOKEN is set, require a matching header (spluft convention).
		if adminToken != "" {
			got := r.Header.Get("X-Admin-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(adminToken)) != 1 {
				writeJSON(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		if h.checkHealth() {
			writeJSON(w, http.StatusOK, "ok")
		} else {
			writeJSON(w, http.StatusServiceUnavailable, "unavailable")
		}
	}
}

func (h *Health) checkHealth() bool {
	if h.PingFunc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.PingFunc(ctx); err != nil {
			return false
		}
	}
	if h.CatalogLoaded != nil && !h.CatalogLoaded() {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
