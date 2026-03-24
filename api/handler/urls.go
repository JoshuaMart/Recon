package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
)

type URLHandler struct {
	queries *db.Queries
}

func NewURLHandler(queries *db.Queries) *URLHandler {
	return &URLHandler{queries: queries}
}

type urlListItem struct {
	ID            string  `json:"id"`
	HostnameID    string  `json:"hostname_id"`
	URL           string  `json:"url"`
	StatusCode    *string `json:"status_code"`
	Title         *string `json:"title"`
	Chain         any     `json:"chain"`
	Technologies  any     `json:"technologies"`
	ExternalHosts any     `json:"external_hosts"`
	ScannedAt     *string `json:"scanned_at"`
}

func (h *URLHandler) List(w http.ResponseWriter, r *http.Request) {
	pg := parsePagination(r)
	q := r.URL.Query()

	params := db.ListWebResultsParams{
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

	if v := q.Get("hostname_id"); v != "" {
		u, err := parseUUID(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid hostname_id")
			return
		}
		params.HostnameID = u
	}

	if v := q.Get("status_code"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5 {
			writeError(w, http.StatusBadRequest, "invalid status_code, must be 1-5 (e.g. 2 for 2xx)")
			return
		}
		params.StatusCodeClass = pgtype.Int4{Int32: int32(n), Valid: true}
	}

	rows, err := h.queries.ListWebResults(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list urls")
		return
	}

	countParams := db.CountWebResultsParams{
		WildcardID:      params.WildcardID,
		HostnameID:      params.HostnameID,
		StatusCodeClass: params.StatusCodeClass,
	}
	total, err := h.queries.CountWebResults(r.Context(), countParams)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count urls")
		return
	}

	items := make([]urlListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, urlListItem{
			ID:            uuidToString(row.ID),
			HostnameID:    uuidToString(row.HostnameID),
			URL:           row.Url,
			StatusCode:    ifaceToStringPtr(row.StatusCode),
			Title:         ifaceToStringPtr(row.Title),
			Chain:         jsonOrNull(row.Chain),
			Technologies:  jsonOrNull(row.Technologies),
			ExternalHosts: jsonOrNull(row.ExternalHosts),
			ScannedAt:     timestampToString(row.ScannedAt),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":     items,
		"total":    total,
		"page":     pg.Page,
		"per_page": pg.PerPage,
	})
}

func (h *URLHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid url ID")
		return
	}

	row, err := h.queries.GetWebResult(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "url not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":             uuidToString(row.ID),
		"hostname_id":    uuidToString(row.HostnameID),
		"url":            row.Url,
		"chain":          jsonOrNull(row.Chain),
		"technologies":   jsonOrNull(row.Technologies),
		"cookies":        jsonOrNull(row.Cookies),
		"metadata":       jsonOrNull(row.Metadata),
		"external_hosts": jsonOrNull(row.ExternalHosts),
		"scanned_at":     timestampToString(row.ScannedAt),
		"created_at":     timestampToStringVal(row.CreatedAt),
		"updated_at":     timestampToStringVal(row.UpdatedAt),
	})
}

type fingerprintRequest struct {
	URL string `json:"url"`
}

func (h *URLHandler) Fingerprint(w http.ResponseWriter, r *http.Request) {
	var req fingerprintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	parsed, err := url.Parse(req.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "invalid URL, must be http(s)://...")
		return
	}

	// Extract hostname from URL
	fqdn := strings.Split(parsed.Host, ":")[0]

	// Find hostname record
	hostname, err := h.queries.GetHostnameByFQDN(r.Context(), fqdn)
	if err != nil {
		writeError(w, http.StatusNotFound, "hostname not found for this URL")
		return
	}

	// Enqueue for fingerprinting
	_, err = h.queries.EnqueueFingerprint(r.Context(), db.EnqueueFingerprintParams{
		Url:        req.URL,
		HostnameID: hostname.ID,
		Source:     "manual",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue fingerprint")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func ifaceToStringPtr(v any) *string {
	if v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}
