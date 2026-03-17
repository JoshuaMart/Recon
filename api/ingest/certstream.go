package ingest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
	"github.com/jomar/recon/api/wildcard"
)

type CertstreamHandler struct {
	queries *db.Queries
}

func NewCertstreamHandler(queries *db.Queries) *CertstreamHandler {
	return &CertstreamHandler{queries: queries}
}

type certstreamRequest struct {
	URL string `json:"url"`
}

func (h *CertstreamHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var req certstreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	parsed, err := url.Parse(req.URL)
	if err != nil || parsed.Host == "" {
		http.Error(w, `{"error":"invalid URL"}`, http.StatusBadRequest)
		return
	}

	fqdn := strings.Split(parsed.Host, ":")[0]

	// Find matching active wildcard
	wildcards, err := h.queries.GetActiveWildcards(r.Context())
	if err != nil {
		http.Error(w, `{"error":"failed to fetch wildcards"}`, http.StatusInternalServerError)
		return
	}

	var matchedWildcard *db.Wildcard
	for _, wc := range wildcards {
		if wildcard.Match(wc.Value, fqdn) {
			matchedWildcard = &wc
			break
		}
	}

	if matchedWildcard == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Check if hostname already exists
	hostname, err := h.queries.GetHostnameByFQDN(r.Context(), fqdn)
	if err != nil {
		// New hostname — create with unreachable/unknown
		hostname, err = h.queries.UpsertHostname(r.Context(), db.UpsertHostnameParams{
			WildcardID: matchedWildcard.ID,
			Fqdn:       fqdn,
			Status:     db.HostnameStatusUnreachable,
			Type:       db.HostnameTypeUnknown,
			LastSeenAt: pgtype.Timestamptz{},
		})
		if err != nil {
			http.Error(w, `{"error":"failed to create hostname"}`, http.StatusInternalServerError)
			return
		}
	}

	// Enqueue URL for fingerprinting
	_, _ = h.queries.EnqueueFingerprint(r.Context(), db.EnqueueFingerprintParams{
		Url:        req.URL,
		HostnameID: hostname.ID,
		Source:     "certstream",
	})

	w.WriteHeader(http.StatusOK)
}
