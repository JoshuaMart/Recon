// Package api serves the HTTP API.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/JoshuaMart/recon/internal/version"
)

// Pinger is what the health route needs from the database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Handler builds the route table.
func Handler(log *slog.Logger, db Pinger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/health", health(db))
	return logRequests(log, mux)
}

type healthResponse struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Database string `json:"database"`
}

// health answers 200 when every component it knows of answers, 503 otherwise.
// Components join it as their phase adds them.
func health(db Pinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		res := healthResponse{Status: "ok", Version: version.Version, Database: "ok"}
		code := http.StatusOK
		if err := db.Ping(ctx); err != nil {
			res.Status, res.Database, code = "degraded", "unreachable", http.StatusServiceUnavailable
		}
		writeJSON(w, code, res)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.LogAttrs(r.Context(), slog.LevelDebug, "http",
			slog.String("method", r.Method), slog.String("path", r.URL.Path),
			slog.Int("status", rec.code), slog.Duration("took", time.Since(start)))
	})
}
