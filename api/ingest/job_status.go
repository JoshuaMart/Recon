package ingest

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
	"github.com/jomar/recon/api/notify"
)

type JobStatusHandler struct {
	queries  *db.Queries
	notifier *notify.Notifier
}

func NewJobStatusHandler(queries *db.Queries, notifier *notify.Notifier) *JobStatusHandler {
	return &JobStatusHandler{queries: queries, notifier: notifier}
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
	case db.JobStatusRunning, db.JobStatusCompleted, db.JobStatusFailed, db.JobStatusTimeout:
		// valid
	default:
		http.Error(w, `{"error":"invalid status, must be running, completed, failed or timeout"}`, http.StatusBadRequest)
		return
	}

	job, err := h.queries.GetJob(r.Context(), id)
	if err != nil {
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

	// Send digest notification when a job completes
	if status == db.JobStatusCompleted {
		go h.sendReconDigest(job)
	}

	w.WriteHeader(http.StatusOK)
}

func (h *JobStatusHandler) sendReconDigest(job db.ReconJob) {
	ctx := context.Background()

	stats, err := h.queries.GetJobCompletionStats(ctx, job.ID)
	if err != nil {
		log.Printf("JobStatus: failed to get completion stats for job: %v", err)
		return
	}

	var duration time.Duration
	if job.StartedAt.Valid {
		duration = time.Since(job.StartedAt.Time)
	}

	h.notifier.NotifyDigest("recon", notify.DigestInfo{
		Kind:               "recon",
		WildcardsProcessed: 1,
		NewHostnames:       int(stats.NewHostnames),
		NewlyDead:          int(stats.NewlyDead),
		NewWebServices:     int(stats.NewWebServices),
		Duration:           duration,
	})
}
