package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/jomar/recon/api/db"
)

type JobHandler struct {
	queries *db.Queries
}

func NewJobHandler(queries *db.Queries) *JobHandler {
	return &JobHandler{queries: queries}
}

func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
	pg := parsePagination(r)
	q := r.URL.Query()

	params := db.ListJobsParams{
		Limit:  int32(pg.PerPage),
		Offset: int32(pg.Offset),
	}

	if v := q.Get("wildcard_id"); v != "" {
		u, err := parseUUID(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid wildcard_id")
			return
		}
		params.WildcardID = u
	}

	if v := q.Get("status"); v != "" {
		params.Status = db.NullJobStatus{JobStatus: db.JobStatus(v), Valid: true}
	}

	rows, err := h.queries.ListJobs(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	countParams := db.CountJobsParams{
		WildcardID: params.WildcardID,
		Status:     params.Status,
	}
	total, err := h.queries.CountJobs(r.Context(), countParams)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count jobs")
		return
	}

	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, jobToMap(row))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":     items,
		"total":    total,
		"page":     pg.Page,
		"per_page": pg.PerPage,
	})
}

func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	row, err := h.queries.GetJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}

	writeJSON(w, http.StatusOK, jobToMap(row))
}

func jobToMap(row db.ReconJob) map[string]any {
	return map[string]any{
		"id":              uuidToString(row.ID),
		"wildcard_id":     uuidToString(row.WildcardID),
		"mode":            string(row.Mode),
		"status":          string(row.Status),
		"scaleway_job_id": textToString(row.ScalewayJobID),
		"started_at":      timestampToString(row.StartedAt),
		"completed_at":    timestampToString(row.CompletedAt),
		"created_at":      timestampToStringVal(row.CreatedAt),
	}
}
