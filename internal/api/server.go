// Package api serves the HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/render"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/version"
)

// Pinger is what the health route needs from the database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Syncer runs a scope sync pass on demand.
type Syncer interface {
	Run(ctx context.Context) (scope.Summary, error)
}

// Deps is what the routes need.
type Deps struct {
	Log   *slog.Logger
	DB    Pinger
	Store *store.Store
	Auth  *Auth
	// Sync is nil when no ScopesExtractor is configured.
	Sync            Syncer
	DefaultAutoScan string
	Now             func() time.Time
	// Storage serves screenshots; Render reports the dispatcher, nil when no
	// Fingerprinter is configured.
	Storage fingerprint.Storage
	Render  func() render.Status
}

type server struct{ Deps }

// Handler builds the route table.
func Handler(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &server{d}
	mux := http.NewServeMux()
	mux.Handle("GET /api/health", s.health())

	if d.Auth != nil {
		mux.HandleFunc("POST /api/session", d.Auth.Login)
		mux.HandleFunc("DELETE /api/session", d.Auth.Logout)
		authed := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(h)) }
		authed("GET /api/programs", s.listPrograms)
		authed("POST /api/programs", s.createProgram)
		authed("GET /api/programs/{id}", s.getProgram)
		authed("PATCH /api/programs/{id}", s.patchProgram)
		authed("POST /api/programs/{id}/rules", s.openRule)
		authed("POST /api/programs/{id}/assets", s.declareAssets)
		authed("PATCH /api/programs/{id}/rules/{rule}", s.patchRule)
		authed("GET /api/wildcards", s.listWildcards)
		authed("POST /api/wildcards/{id}/recon", s.reconWildcard)
		authed("POST /api/wildcards/recon", s.reconOldest)
		authed("GET /api/runs", s.listRuns)
		authed("GET /api/runs/{id}", s.getRun)
		authed("GET /api/search", s.search)
		authed("GET /api/search/facets", s.searchFacets)
		authed("GET /api/search/fields", s.searchFields)
		authed("GET /api/search/export", s.searchExport)
		authed("GET /api/assets/{id}", s.getAsset)
		authed("GET /api/assets/{id}/changes", s.assetChanges)
		authed("PATCH /api/assets/{id}/triage", s.patchTriage)
		authed("POST /api/assets/{id}/render", s.renderAsset)
		authed("GET /api/assets/{id}/screenshots", s.listScreenshots)
		authed("GET /api/screenshots/{key}", s.getScreenshot)
		authed("POST /api/sync", s.runSync)
		mux.Handle("/api/", d.Auth.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such route")
		})))
	}
	return logRequests(d.Log, mux)
}

type healthResponse struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Database string `json:"database"`
	Sync     string `json:"sync,omitempty"`
	Render   string `json:"render,omitempty"`
	// RenderQueue is the number of renders due, per priority: high, normal, low.
	RenderQueue map[string]int `json:"render_queue,omitempty"`
}

// health answers 200 when the database answers, 503 otherwise. It is public,
// so it says how each component is, never why.
func (s *server) health() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		res := healthResponse{Status: "ok", Version: version.Version, Database: "ok"}
		code := http.StatusOK
		if err := s.DB.Ping(ctx); err != nil {
			res.Status, res.Database, code = "degraded", "unreachable", http.StatusServiceUnavailable
		} else if s.Store != nil {
			res.Sync = s.syncHealth(ctx)
			res.Render, res.RenderQueue = s.renderHealth(ctx)
			if res.Sync == "failing" || res.Render == "unreachable" {
				res.Status = "degraded"
			}
		}
		writeJSON(w, code, res)
	})
}

func (s *server) syncHealth(ctx context.Context) string {
	if s.Sync == nil {
		return "disabled"
	}
	st, err := s.Store.Q.GetSyncState(ctx)
	switch {
	case err != nil:
		return "unknown"
	case st.LastErrorAt != nil && (st.LastSuccessAt == nil || st.LastErrorAt.After(*st.LastSuccessAt)):
		return "failing"
	case st.LastSuccessAt == nil:
		return "pending"
	}
	return "ok"
}

func (s *server) renderHealth(ctx context.Context) (string, map[string]int) {
	if s.Render == nil {
		return "disabled", nil
	}
	state := "unreachable"
	if s.Render().Healthy {
		state = "ok"
	}
	rows, err := s.Store.Q.RenderQueueDepth(ctx, s.now())
	if err != nil {
		return state, nil
	}
	names := map[int16]string{2: "high", 1: "normal", 0: "low"}
	q := map[string]int{"high": 0, "normal": 0, "low": 0}
	for _, r := range rows {
		q[names[r.RenderPriority]] = int(r.Depth)
	}
	return state, q
}

type errorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

func writeError(w http.ResponseWriter, code int, kind, detail string) {
	writeJSON(w, code, errorBody{Error: kind, Detail: detail})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// readJSON decodes a bounded body and refuses unknown fields, so a typo is an
// error rather than a silent no-op.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return false
	}
	return true
}

// pathID reads an identifier from the path. A malformed one answers 404 like
// an unknown one.
func pathID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", name+" not found")
		return uuid.UUID{}, false
	}
	return id, true
}

func (s *server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "see the server log")
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
