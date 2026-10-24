package api

import (
	"errors"
	"net"
	"net/http"
	"slices"
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

// changesPage is a page of the feed.
const changesPage = 100

func (s *server) feedRequest(w http.ResponseWriter, q, view, run, cursor string) (search.FeedRequest, bool) {
	req := search.FeedRequest{Query: q, Cursor: cursor, Limit: changesPage, Now: s.now()}
	switch view {
	case "", "unseen":
		req.Unseen = true
	case "all":
	default:
		writeError(w, http.StatusBadRequest, "bad_field", "view is unseen or all")
		return req, false
	}
	if run != "" {
		id, err := uuid.Parse(run)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_field", "run is not an id")
			return req, false
		}
		req.Run = &id
	}
	return req, true
}

func (s *server) listChanges(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	req, ok := s.feedRequest(w, v.Get("q"), v.Get("view"), v.Get("run"), v.Get("cursor"))
	if !ok {
		return
	}
	page, err := search.Feed(r.Context(), s.Store.Pool, req)
	if err != nil {
		s.searchError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *server) unseenChanges(w http.ResponseWriter, r *http.Request) {
	n, err := search.UnseenCount(r.Context(), s.Store.Pool)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unseen": n, "capped": n >= search.MaxUnseen})
}

// markChangesSeen marks as seen what a feed read shows: the assets behind its
// unseen changes, as opening each of them would.
func (s *server) markChangesSeen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Q   string `json:"q"`
		Run string `json:"run"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	req, ok := s.feedRequest(w, body.Q, "unseen", body.Run, "")
	if !ok {
		return
	}
	n, err := search.MarkSeen(r.Context(), s.Store.Pool, req)
	if err != nil {
		s.searchError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": n})
}

func (s *server) listNotifications(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.RecentNotifications(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if rows == nil {
		rows = []sqlcgen.Notification{}
	}
	channels := s.Channels
	if channels == nil {
		channels = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": rows, "channels": channels})
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
	s.evaluateSoon(row.Alert)
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
		wasAlert := cur.Alert
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
		if err != nil {
			return 0, nil, err
		}
		// Off, the matches go: switched on again, it starts over.
		if wasAlert && !row.Alert {
			if err := q.ClearMatches(r.Context(), id); err != nil {
				return 0, nil, err
			}
		}
		s.evaluateSoon(row.Alert)
		return http.StatusOK, row, nil
	})
}

// evaluateSoon records an alerting search's matches without delay, so what
// enters after it was switched on is not taken for what it already matched.
func (s *server) evaluateSoon(alert bool) {
	if alert && s.Evaluate != nil {
		s.Evaluate()
	}
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
