package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
)

var wildcardPattern = regexp.MustCompile(`^\*\.[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?(\.[a-zA-Z]{2,})+$`)

type JobLauncher interface {
	LaunchJob(wildcardValue, jobID, mode string) (scalewayJobID string, err error)
}

type JobStatusChecker interface {
	GetJobStatus(jobRunID string) (string, error)
}

type RevalidateFunc func(ctx context.Context, wc db.Wildcard)

type WildcardHandler struct {
	queries    *db.Queries
	launcher   JobLauncher
	revalidate RevalidateFunc
}

func NewWildcardHandler(queries *db.Queries, launcher JobLauncher, revalidate RevalidateFunc) *WildcardHandler {
	return &WildcardHandler{queries: queries, launcher: launcher, revalidate: revalidate}
}

type wildcardResponse struct {
	ID                string  `json:"id"`
	Value             string  `json:"value"`
	Active            bool    `json:"active"`
	LastReconAt       *string `json:"last_recon_at"`
	LastRevalidatedAt *string `json:"last_revalidated_at"`
	Stats             *stats  `json:"stats,omitempty"`
}

type stats struct {
	HostnamesTotal       int64 `json:"hostnames_total"`
	HostnamesAlive       int64 `json:"hostnames_alive"`
	HostnamesDead        int64 `json:"hostnames_dead"`
	HostnamesUnreachable int64 `json:"hostnames_unreachable"`
	WebServices          int64 `json:"web_services"`
}

func (h *WildcardHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.queries.ListWildcards(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list wildcards")
		return
	}

	result := make([]wildcardResponse, 0, len(rows))
	for _, row := range rows {
		result = append(result, wildcardResponse{
			ID:                uuidToString(row.ID),
			Value:             row.Value,
			Active:            row.Active,
			LastReconAt:       timestampToString(row.LastReconAt),
			LastRevalidatedAt: timestampToString(row.LastRevalidatedAt),
			Stats: &stats{
				HostnamesTotal:       row.HostnamesTotal,
				HostnamesAlive:       row.HostnamesAlive,
				HostnamesDead:        row.HostnamesDead,
				HostnamesUnreachable: row.HostnamesUnreachable,
				WebServices:          row.WebServices,
			},
		})
	}

	writeJSON(w, http.StatusOK, result)
}

type createWildcardRequest struct {
	Value string `json:"value"`
}

func (h *WildcardHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createWildcardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !wildcardPattern.MatchString(req.Value) {
		writeError(w, http.StatusBadRequest, "invalid wildcard format, expected *.domain.tld")
		return
	}

	wc, err := h.queries.InsertWildcard(r.Context(), req.Value)
	if err != nil {
		writeError(w, http.StatusConflict, "wildcard already exists")
		return
	}

	writeJSON(w, http.StatusCreated, wildcardResponse{
		ID:     uuidToString(wc.ID),
		Value:  wc.Value,
		Active: wc.Active,
	})
}

func (h *WildcardHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid wildcard ID")
		return
	}

	if err := h.queries.DeleteWildcard(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete wildcard")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type launchReconRequest struct {
	Mode string `json:"mode"`
}

func (h *WildcardHandler) LaunchRecon(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid wildcard ID")
		return
	}

	var req launchReconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Mode = "normal"
	}
	if req.Mode == "" {
		req.Mode = "normal"
	}
	if req.Mode != "normal" && req.Mode != "intensive" {
		writeError(w, http.StatusBadRequest, "mode must be 'normal' or 'intensive'")
		return
	}

	// Check wildcard exists
	wc, err := h.queries.GetWildcard(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "wildcard not found")
		return
	}

	// Check no active job for this wildcard
	hasActive, err := h.queries.HasActiveJobForWildcard(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check active jobs")
		return
	}
	if hasActive {
		writeError(w, http.StatusConflict, "a job is already running for this wildcard")
		return
	}

	// Check global concurrency limit
	activeCount, err := h.queries.CountActiveJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count active jobs")
		return
	}
	if activeCount >= 10 {
		writeError(w, http.StatusConflict, "maximum concurrent jobs reached (10)")
		return
	}

	// Create job record
	job, err := h.queries.InsertJob(r.Context(), db.InsertJobParams{
		WildcardID: id,
		Mode:       db.ReconMode(req.Mode),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return
	}

	// Launch Scaleway job
	var scalewayJobID *string
	if h.launcher != nil {
		scwID, err := h.launcher.LaunchJob(wc.Value, uuidToString(job.ID), req.Mode)
		if err != nil {
			// Update job to failed
			_ = h.queries.UpdateJobStatus(r.Context(), db.UpdateJobStatusParams{
				ID:     job.ID,
				Status: db.JobStatusFailed,
			})
			writeError(w, http.StatusInternalServerError, "failed to launch scaleway job")
			return
		}
		scalewayJobID = &scwID
		_ = h.queries.UpdateJobScalewayID(r.Context(), db.UpdateJobScalewayIDParams{
			ID:            job.ID,
			ScalewayJobID: pgtype.Text{String: scwID, Valid: true},
		})
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":          uuidToString(job.ID),
		"scaleway_job_id": scalewayJobID,
		"status":          string(job.Status),
	})
}

func (h *WildcardHandler) Revalidate(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid wildcard ID")
		return
	}

	wc, err := h.queries.GetWildcard(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "wildcard not found")
		return
	}

	if h.revalidate != nil {
		go h.revalidate(context.Background(), wc)
	}

	w.WriteHeader(http.StatusAccepted)
}

// Helpers

func uuidToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return u, err
	}
	return u, nil
}

func timestampToString(ts pgtype.Timestamptz) *string {
	if !ts.Valid {
		return nil
	}
	s := ts.Time.Format("2006-01-02T15:04:05Z07:00")
	return &s
}
