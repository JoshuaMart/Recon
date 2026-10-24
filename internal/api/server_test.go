package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func TestHealth(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name     string
		db       fakeDB
		code     int
		database string
	}{
		{"database answers", fakeDB{}, http.StatusOK, "ok"},
		{"database down", fakeDB{err: errors.New("refused")}, http.StatusServiceUnavailable, "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler(Deps{Log: log, DB: tc.db}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", nil))

			if rec.Code != tc.code {
				t.Errorf("status = %d, want %d", rec.Code, tc.code)
			}
			var body healthResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Database != tc.database {
				t.Errorf("database = %q, want %q", body.Database, tc.database)
			}
		})
	}
}

func TestHealthRefusesOtherMethods(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := httptest.NewRecorder()
	Handler(Deps{Log: log, DB: fakeDB{}}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestUnknownAPIPathIsNotThePage(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<!doctype html>")) })
	rec := httptest.NewRecorder()
	Handler(Deps{Log: log, DB: fakeDB{}, UI: page}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/typo", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("GET /api/typo = %d %q, want a JSON 404", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	Handler(Deps{Log: log, DB: fakeDB{}, UI: page}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/wildcards", nil))
	if rec.Body.String() != "<!doctype html>" {
		t.Errorf("GET /wildcards = %q, want the page", rec.Body.String())
	}
}
