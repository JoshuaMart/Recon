package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/JoshuaMart/recon/internal/search"
)

func (s *server) searchRequest(r *http.Request) search.Request {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit == 0 {
		limit = 50
	}
	return search.Request{Query: q.Get("q"), View: q.Get("view"), Cursor: q.Get("cursor"), Limit: limit, Now: s.now()}
}

// searchError answers a query the language refuses, naming what it refused:
// an empty page would read as an empty inventory.
func (s *server) searchError(w http.ResponseWriter, r *http.Request, err error) {
	var se *search.SyntaxError
	var qe *search.QueryError
	switch {
	case errors.As(err, &se):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "syntax_error", "detail": se.Error(), "position": se.Pos})
	case errors.As(err, &qe):
		body := map[string]any{"error": qe.Kind, "detail": qe.Detail, "field": qe.Field}
		if qe.Suggest != "" {
			body["suggest"] = qe.Suggest
		}
		writeJSON(w, http.StatusBadRequest, body)
	case errors.Is(err, search.ErrBadCursor):
		writeError(w, http.StatusBadRequest, "bad_cursor", err.Error())
	default:
		s.internal(w, r, err)
	}
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	page, err := search.Search(r.Context(), s.Store.Pool, s.searchRequest(r))
	if err != nil {
		s.searchError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *server) searchFacets(w http.ResponseWriter, r *http.Request) {
	f, err := search.Facets(r.Context(), s.Store.Pool, s.searchRequest(r))
	if err != nil {
		s.searchError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"facets": f})
}

func (s *server) searchFields(w http.ResponseWriter, _ *http.Request) {
	f, has := search.Fields()
	writeJSON(w, http.StatusOK, map[string]any{"fields": f, "has": has})
}

func (s *server) searchExport(w http.ResponseWriter, r *http.Request) {
	req := s.searchRequest(r)
	format := r.URL.Query().Get("format")
	// The query is compiled before anything is written, so an unknown field is
	// still a 400 and never an empty 200 that reads as an empty inventory.
	if err := search.Validate(req); err != nil {
		s.searchError(w, r, err)
		return
	}
	var write func(search.Row) error
	var flush func()
	switch format {
	case "", "json":
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		write = func(row search.Row) error { return enc.Encode(row) }
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"kind", "key", "url", "program", "state", "status", "title", "technologies", "first_seen"})
		write = func(row search.Row) error {
			status := ""
			if row.StatusCode != nil {
				status = strconv.Itoa(int(*row.StatusCode))
			}
			return cw.Write([]string{
				row.Kind, row.Key, row.URL(), row.Program, row.Lifecycle, status,
				deref(row.Title), strings.Join(row.Technologies, " "), row.FirstSeen.Format("2006-01-02T15:04:05Z07:00"),
			})
		}
		flush = cw.Flush
	case "urls", "hosts":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		seen := map[string]bool{}
		write = func(row search.Row) error {
			v := row.Host
			if format == "urls" {
				if v = row.URL(); v == "" {
					return nil
				}
			}
			if seen[v] {
				return nil
			}
			seen[v] = true
			_, err := w.Write([]byte(v + "\n"))
			return err
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_field", "format is json, csv, urls or hosts")
		return
	}
	light := format == "urls" || format == "hosts"
	if err := search.Export(r.Context(), s.Store.Pool, req, light, write); err != nil {
		// Headers are sent: the client sees a truncated body, the log says why.
		s.Log.ErrorContext(r.Context(), "export", "err", err)
	}
	if flush != nil {
		flush()
	}
}
