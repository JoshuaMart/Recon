package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store"
)

// maxQueryBytes bounds a filter tree.
//
// A tree is a few hundred bytes and the compiler already bounds its depth and
// its clause count. This is the layer in front of both, so a body that would
// exhaust the decoder never reaches them.
const maxQueryBytes = 1 << 20

// Assets is the search surface.
//
// A structured tree rather than a query string, and a POST rather than a GET,
// because the question is a document. A textual language comes later and
// produces the same tree, which is what avoids freezing a syntax before anybody
// knows what actually gets filtered.
type Assets struct {
	db  *store.Scoped
	log *slog.Logger
}

// NewAssets builds it.
func NewAssets(db *store.Scoped, log *slog.Logger) *Assets {
	return &Assets{db: db, log: log}
}

// query is the body every route here takes.
type query struct {
	Filter json.RawMessage `json:"filter"`
	Limit  int             `json:"limit,omitempty"`
	Cursor string          `json:"cursor,omitempty"`
	Format string          `json:"format,omitempty"`
}

// Search answers one page.
func (h *Assets) Search(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	body, filter, ok := h.read(w, r)
	if !ok {
		return
	}
	cursor, err := search.ParseCursor(body.Cursor)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_cursor", err.Error())
		return
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.log.ErrorContext(ctx, "begin failed", "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the search could not be run")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	page, err := search.List(ctx, tx, principal.OrgID, search.Request{
		Filter: filter, Limit: body.Limit, Cursor: cursor, Display: true,
	})
	if err != nil {
		h.answerError(ctx, w, "search failed", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// Facets aggregates over the filtered result rather than over the inventory.
func (h *Assets) Facets(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	_, filter, ok := h.read(w, r)
	if !ok {
		return
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.log.ErrorContext(ctx, "begin failed", "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the facets could not be computed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	facets, err := search.Facets(ctx, tx, principal.OrgID, filter)
	if err != nil {
		h.answerError(ctx, w, "facets failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"facets": facets})
}

// Export writes the filtered result in full, as it walks it.
//
// It streams rather than accumulating, so the response starts before the walk
// finishes and nothing holds an inventory in memory. The consequence is worth
// stating: a failure halfway through has already sent a valid prefix, and the
// status line is long gone. That is why the count is logged and why a limit the
// caller asked for is honoured exactly rather than rounded.
func (h *Assets) Export(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	body, filter, ok := h.read(w, r)
	if !ok {
		return
	}
	format := body.Format
	if format == "" {
		format = search.FormatJSONL
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.log.ErrorContext(ctx, "begin failed", "error", err)
		fail(w, http.StatusInternalServerError, "unavailable", "the export could not be run")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The status is committed to only once the first page has come back, which
	// is what lets a failure on the very first statement stay a failure. Sent
	// up front, it would answer 200 with a zero byte body, and the caller has
	// no way to tell that from an inventory with nothing in it.
	started := false
	begin := func() io.Writer {
		started = true
		contentType := "application/x-ndjson"
		if format == search.FormatCSV {
			contentType = "text/csv; charset=utf-8"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return w
	}

	written, err := search.Export(ctx, tx, principal.OrgID, filter, format, body.Limit, begin)
	if err != nil {
		if !started {
			h.answerError(ctx, w, "export failed before it began", err)
			return
		}
		// Past that point the body has begun and this cannot become a status.
		// It is logged and the connection carries a truncated file, which is
		// the one case where an export ends short: a failure rather than a cap.
		h.log.ErrorContext(ctx, "export failed part way",
			"org", principal.OrgID, "written", written, "error", err)
		return
	}
	h.log.InfoContext(ctx, "export written",
		"org", principal.OrgID, "format", format, "assets", written)
}

// Fields says what the search accepts.
//
// Served rather than deduced, for the same reason the enrichment state is: a
// console learning a vocabulary against 400s learns it wrong.
func (h *Assets) Fields(w http.ResponseWriter, _ *http.Request, _ auth.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{
		"fields":       search.Fields(),
		"facets":       search.FacetLimit,
		"page_limit":   search.MaxLimit,
		"page_default": search.DefaultLimit,
	})
}

func (h *Assets) read(w http.ResponseWriter, r *http.Request) (query, search.Node, bool) {
	var body query
	if r.ContentLength != 0 {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxQueryBytes))
		if err := decoder.Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "malformed", "the body is not a search request")
			return query{}, search.Node{}, false
		}
	}

	filter, err := search.Parse(body.Filter)
	if err != nil {
		// Named rather than answered with an empty result set, which a console
		// would read as an empty inventory.
		fail(w, http.StatusBadRequest, "bad_filter", err.Error())
		return query{}, search.Node{}, false
	}
	return body, filter, true
}

// answerError tells a refusal from a failure.
//
// A query the registry does not describe is the caller's, and naming it is what
// makes the vocabulary learnable. Anything else is ours and says nothing about
// the database to the outside.
func (h *Assets) answerError(ctx context.Context, w http.ResponseWriter, what string, err error) {
	var refusal *search.Error
	if errors.As(err, &refusal) {
		fail(w, http.StatusBadRequest, "bad_filter", refusal.Error())
		return
	}
	h.log.ErrorContext(ctx, what, "error", err)
	fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
}
