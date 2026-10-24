package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/runs"
)

// maxReport bounds a report body: a large perimeter makes a large document,
// but a job must not be able to exhaust the process reading it.
const maxReport = 64 << 20

// Jobs is what the routes a remote job calls need from the scheduler.
type Jobs interface {
	Targets(ctx context.Context, id uuid.UUID) ([]string, error)
	Deliver(ctx context.Context, id uuid.UUID, raw []byte) (ingest.Summary, error)
}

// jobAuth checks a job's token for its run and purpose. It comes first and
// answers alike for every failure, so the routes say nothing of a run to
// whoever holds no token for it.
func (s *server) jobAuth(w http.ResponseWriter, r *http.Request, p runs.Purpose) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err != nil || !ok || s.RunTokens.Check(token, p, id, s.now()) != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing, wrong or expired run token")
		return uuid.UUID{}, false
	}
	return id, true
}

func (s *server) jobError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, runs.ErrNotARun):
		writeError(w, http.StatusNotFound, "not_found", "no such job run")
	case errors.Is(err, runs.ErrRunClosed):
		// A second copy of an ingested report, or one past its run's end.
		writeError(w, http.StatusConflict, "run_closed", "the run is no longer running, the report is not ingested")
	case errors.Is(err, runs.ErrBadReport):
		writeError(w, http.StatusBadRequest, "bad_report", err.Error())
	default:
		s.internal(w, r, err)
	}
}

// runTargets serves a verification job its host list, one per line.
func (s *server) runTargets(w http.ResponseWriter, r *http.Request) {
	id, ok := s.jobAuth(w, r, runs.PurposeTargets)
	if !ok {
		return
	}
	targets, err := s.RunJobs.Targets(r.Context(), id)
	if err != nil {
		s.jobError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, strings.Join(targets, "\n")+"\n") //nolint:gosec // plain text with nosniff, canonical host names read by FastRecon
}

// runReport takes the report a job posts.
func (s *server) runReport(w http.ResponseWriter, r *http.Request) {
	id, ok := s.jobAuth(w, r, runs.PurposeReport)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReport))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the report is larger than the limit")
		return
	}
	sum, err := s.RunJobs.Deliver(r.Context(), id, raw)
	if err != nil {
		s.jobError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
