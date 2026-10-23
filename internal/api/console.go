package api

import (
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// The routes the console needs beyond the inventory: the changes feed, probe
// now, saved searches and the settings page.

const changesPage = 100

type changeView struct {
	ID        int64      `json:"id"`
	AssetID   uuid.UUID  `json:"asset_id"`
	ProgramID uuid.UUID  `json:"program_id"`
	Program   string     `json:"program"`
	AssetKind string     `json:"asset_kind"`
	AssetKey  string     `json:"asset_key"`
	Kind      string     `json:"kind"`
	Layer     *string    `json:"layer"`
	Before    any        `json:"before"`
	After     any        `json:"after"`
	RunID     *uuid.UUID `json:"run_id"`
	At        time.Time  `json:"at"`
}

// changeCursor is the last row of a page, (at, id), opaque to the caller.
func changeCursor(at time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)))
}

func parseChangeCursor(c string) (time.Time, int64, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, 0, false
	}
	ts, idStr, ok := strings.Cut(string(b), "|")
	if !ok {
		return time.Time{}, 0, false
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, 0, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	return at, id, err == nil
}

func (s *server) listChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params := sqlcgen.ListChangesParams{Lim: changesPage}
	if v := q.Get("program"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_field", "program is not an id")
			return
		}
		params.ProgramID = &id
	}
	if c := q.Get("cursor"); c != "" {
		at, id, ok := parseChangeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_cursor", "cursor does not decode")
			return
		}
		params.BeforeAt, params.BeforeID = &at, &id
	}
	rows, err := s.Store.Q.ListChanges(r.Context(), params)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]changeView, 0, len(rows))
	for _, c := range rows {
		out = append(out, changeView{
			ID: c.ID, AssetID: c.AssetID, ProgramID: c.ProgramID, Program: c.Program, AssetKind: c.AssetKind,
			AssetKey: c.AssetKey, Kind: c.Kind, Layer: c.Layer, Before: raw(c.Before), After: raw(c.After),
			RunID: c.RunID, At: c.At,
		})
	}
	res := map[string]any{"changes": out}
	if len(rows) == changesPage {
		last := rows[len(rows)-1]
		res["next"] = changeCursor(last.At, last.ID)
	}
	writeJSON(w, http.StatusOK, res)
}

// probeAsset puts the asset's host first in line for a full. Verification is the
// only prober: with it off, or for a host that gets no due dates, the answer
// says why nothing will happen.
func (s *server) probeAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	a, err := s.Store.Q.GetAsset(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "id not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !s.Verification {
		writeJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": "verification is disabled"})
		return
	}
	n, err := s.Store.Q.ProbeHostNow(r.Context(), sqlcgen.ProbeHostNowParams{ProgramID: a.ProgramID, Host: a.Host})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if n == 0 {
		reason := "the host is out of scope, archived, or its program is not active"
		if net.ParseIP(a.Host) != nil {
			reason = "only named hosts are verified"
		}
		writeJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true, "host": a.Host})
}

type savedSearchBody struct {
	Name  *string `json:"name"`
	Query *string `json:"query"`
	View  *string `json:"view"`
	Alert *bool   `json:"alert"`
}

// apply merges the body into cur and checks the result: a saved query the
// language refuses would fail every time it is opened.
func (b savedSearchBody) apply(cur *sqlcgen.SavedSearch, now time.Time) (string, bool) {
	if b.Name != nil {
		cur.Name = strings.TrimSpace(*b.Name)
	}
	if b.Query != nil {
		cur.Query = strings.TrimSpace(*b.Query)
	}
	if b.View != nil {
		cur.View = *b.View
	}
	if b.Alert != nil {
		cur.Alert = *b.Alert
	}
	switch {
	case cur.Name == "":
		return "name is required", false
	case !slices.Contains([]string{"services", "hosts"}, cur.View):
		return "view is services or hosts", false
	}
	if err := search.Validate(search.Request{Query: cur.Query, View: cur.View, Limit: 1, Now: now}); err != nil {
		return "query: " + err.Error(), false
	}
	return "", true
}

func (s *server) listSavedSearches(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.ListSavedSearches(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if rows == nil {
		rows = []sqlcgen.SavedSearch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved_searches": rows})
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (s *server) createSavedSearch(w http.ResponseWriter, r *http.Request) {
	var body savedSearchBody
	if !readJSON(w, r, &body) {
		return
	}
	cur := sqlcgen.SavedSearch{View: "services"}
	if msg, ok := body.apply(&cur, s.now()); !ok {
		writeError(w, http.StatusBadRequest, "bad_field", msg)
		return
	}
	row, err := s.Store.Q.CreateSavedSearch(r.Context(), sqlcgen.CreateSavedSearchParams{
		Name: cur.Name, Query: cur.Query, View: cur.View, Alert: cur.Alert, Now: s.now(),
	})
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "exists", "a saved search has that name")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *server) patchSavedSearch(w http.ResponseWriter, r *http.Request) {
	var body savedSearchBody
	if !readJSON(w, r, &body) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.inTx(w, r, func(q *sqlcgen.Queries) (int, any, error) {
		cur, err := q.GetSavedSearch(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "id not found")
			return 0, nil, errHandled
		}
		if err != nil {
			return 0, nil, err
		}
		if msg, ok := body.apply(&cur, s.now()); !ok {
			writeError(w, http.StatusBadRequest, "bad_field", msg)
			return 0, nil, errHandled
		}
		row, err := q.UpdateSavedSearch(r.Context(), sqlcgen.UpdateSavedSearchParams{
			ID: id, Name: cur.Name, Query: cur.Query, View: cur.View, Alert: cur.Alert, Now: s.now(),
		})
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "exists", "a saved search has that name")
			return 0, nil, errHandled
		}
		return http.StatusOK, row, err
	})
}

func (s *server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	n, err := s.Store.Q.DeleteSavedSearch(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "id not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// settings is the config in effect, which the caller hands over without its
// secrets, and the space screenshots take.
func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Q.ScreenshotStorage(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config":      s.Settings,
		"screenshots": map[string]any{"count": st.Count, "bytes": st.Bytes},
	})
}
