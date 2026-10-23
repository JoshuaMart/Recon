package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// renderPriorityHigh is what you asking for a render gets: it passes every
// burst.
const renderPriorityHigh = 2

func (s *server) renderAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	cur, err := s.Store.Q.GetAssetCurrent(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "id not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	n, err := s.Store.Q.TriggerRender(r.Context(), sqlcgen.TriggerRenderParams{AssetID: id, Priority: renderPriorityHigh, Now: s.now()})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if n == 0 {
		// A 200 is not always a yes: say why nothing will render, rather than
		// let the caller wait for a render nobody makes.
		writeJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": renderRefusal(cur)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true})
}

func renderRefusal(cur sqlcgen.AssetCurrent) string {
	switch {
	case cur.Kind != "service" && cur.Kind != "url":
		return "only web services and declared URLs are rendered"
	case cur.ScopeStatus != "in_scope":
		return "the asset is " + strings.ReplaceAll(cur.ScopeStatus, "_", " ")
	case cur.Lifecycle == "archived":
		return "the asset is archived"
	case cur.Kind == "service" && cur.HttpState == nil:
		return "the service has not answered HTTP yet"
	}
	return "the program is not active"
}

type screenshotView struct {
	ID      int64     `json:"id"`
	URL     string    `json:"url"`
	Thumb   string    `json:"thumbnail_url"`
	Bytes   int32     `json:"bytes"`
	Width   int32     `json:"width"`
	Height  int32     `json:"height"`
	TakenAt time.Time `json:"taken_at"`
}

func (s *server) listScreenshots(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.Store.Q.ListScreenshots(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]screenshotView, 0, len(rows))
	for _, row := range rows {
		out = append(out, screenshotView{
			ID: row.ID, URL: "/api/screenshots/" + row.Key, Thumb: "/api/screenshots/" + row.ThumbKey,
			Bytes: row.Bytes, Width: row.Width, Height: row.Height, TakenAt: row.TakenAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"screenshots": out})
}

func (s *server) getScreenshot(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if s.Storage == nil || !strings.HasSuffix(key, ".jpg") {
		writeError(w, http.StatusNotFound, "not_found", "no such screenshot")
		return
	}
	// Only keys a row names are served: storage is not a file browser.
	known, err := s.Store.Q.ScreenshotKeyInUse(r.Context(), key)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !known {
		writeError(w, http.StatusNotFound, "not_found", "no such screenshot")
		return
	}
	b, err := s.Storage.Get(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such screenshot")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// A screenshot of a target's page: never let a browser sniff it into HTML.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	// Content-addressed: a key's bytes never change.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = w.Write(b) //nolint:gosec // re-encoded JPEG served as image/jpeg with nosniff
}
