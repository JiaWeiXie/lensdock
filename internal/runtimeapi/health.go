package runtimeapi

import (
	"log/slog"
	"net/http"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// NewHealthHandler serves only GET /healthz and GET /readyz without detail.
// The runtime mounts it on a loopback-only plain listener while the API
// listener is mTLS-only, so the container healthcheck keeps working without
// exposing any operator or lease route outside mTLS.
func NewHealthHandler(health HealthSource, log *slog.Logger) http.Handler {
	if health == nil {
		panic("runtimeapi: Health is required")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready := health.Ready()
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]bool{"ready": ready})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "no such route")
	})
	return middleware(log, mux)
}
