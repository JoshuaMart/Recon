package ingest

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
)

type JobStatusHandler struct {
	queries *db.Queries
}

func NewJobStatusHandler(queries *db.Queries) *JobStatusHandler {
	return &JobStatusHandler{queries: queries}
}

type jobStatusRequest struct {
	Status string `json:"status"`
}

func (h *JobStatusHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var id pgtype.UUID
	if err := id.Scan(chi.URLParam(r, "id")); err != nil {
		http.Error(w, `{"error":"invalid job ID"}`, http.StatusBadRequest)
		return
	}

	var req jobStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	status := db.JobStatus(req.Status)
	switch status {
	case db.JobStatusRunning, db.JobStatusCompleted, db.JobStatusFailed:
		// valid
	default:
		http.Error(w, `{"error":"invalid status, must be running, completed or failed"}`, http.StatusBadRequest)
		return
	}

	if _, err := h.queries.GetJob(r.Context(), id); err != nil {
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
		return
	}

	if err := h.queries.UpdateJobStatus(r.Context(), db.UpdateJobStatusParams{
		ID:     id,
		Status: status,
	}); err != nil {
		http.Error(w, `{"error":"failed to update job status"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
