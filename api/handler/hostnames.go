package handler

import (
	"encoding/json"
	"net/http"
	"net/netip"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
)

type HostnameHandler struct {
	queries *db.Queries
}

func NewHostnameHandler(queries *db.Queries) *HostnameHandler {
	return &HostnameHandler{queries: queries}
}

type hostnameListItem struct {
	ID         string          `json:"id"`
	WildcardID string          `json:"wildcard_id"`
	FQDN       string          `json:"fqdn"`
	IP         *string         `json:"ip"`
	CDN        *string         `json:"cdn"`
	Status     string          `json:"status"`
	Type       string          `json:"type"`
	Ports      json.RawMessage `json:"ports"`
	LastSeenAt *string         `json:"last_seen_at"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
}

type hostnameDetail struct {
	hostnameListItem
	DNS json.RawMessage `json:"dns"`
}

func (h *HostnameHandler) List(w http.ResponseWriter, r *http.Request) {
	pg := parsePagination(r)
	q := r.URL.Query()

	params := db.ListHostnamesParams{
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
		params.Status = db.NullHostnameStatus{HostnameStatus: db.HostnameStatus(v), Valid: true}
	}

	if v := q.Get("type"); v != "" {
		params.Type = db.NullHostnameType{HostnameType: db.HostnameType(v), Valid: true}
	}

	rows, err := h.queries.ListHostnames(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list hostnames")
		return
	}

	countParams := db.CountHostnamesParams{
		WildcardID: params.WildcardID,
		Status:     params.Status,
		Type:       params.Type,
	}
	total, err := h.queries.CountHostnames(r.Context(), countParams)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count hostnames")
		return
	}

	items := make([]hostnameListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, toHostnameListItem(row))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":     items,
		"total":    total,
		"page":     pg.Page,
		"per_page": pg.PerPage,
	})
}

func (h *HostnameHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid hostname ID")
		return
	}

	row, err := h.queries.GetHostname(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "hostname not found")
		return
	}

	detail := hostnameDetail{
		hostnameListItem: hostnameListItem{
			ID:         uuidToString(row.ID),
			WildcardID: uuidToString(row.WildcardID),
			FQDN:       row.Fqdn,
			IP:         addrToString(row.Ip),
			CDN:        textToString(row.Cdn),
			Status:     string(row.Status),
			Type:       string(row.Type),
			Ports:      jsonOrNull(row.Ports),
			LastSeenAt: timestampToString(row.LastSeenAt),
			CreatedAt:  timestampToStringVal(row.CreatedAt),
			UpdatedAt:  timestampToStringVal(row.UpdatedAt),
		},
		DNS: jsonOrNull(row.Dns),
	}

	writeJSON(w, http.StatusOK, detail)
}

func toHostnameListItem(row db.ListHostnamesRow) hostnameListItem {
	return hostnameListItem{
		ID:         uuidToString(row.ID),
		WildcardID: uuidToString(row.WildcardID),
		FQDN:       row.Fqdn,
		IP:         addrToString(row.Ip),
		CDN:        textToString(row.Cdn),
		Status:     string(row.Status),
		Type:       string(row.Type),
		Ports:      jsonOrNull(row.Ports),
		LastSeenAt: timestampToString(row.LastSeenAt),
		CreatedAt:  timestampToStringVal(row.CreatedAt),
		UpdatedAt:  timestampToStringVal(row.UpdatedAt),
	}
}

func textToString(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func addrToString(a *netip.Addr) *string {
	if a == nil {
		return nil
	}
	s := a.String()
	return &s
}

func timestampToStringVal(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.Format("2006-01-02T15:04:05Z07:00")
}

func jsonOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}
