package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// genericPrograms is the number of programs from which a shared value is
// everywhere rather than a link: a CDN script, PHPSESSID.
const genericPrograms = 5

type pivot struct {
	Kind     string `json:"kind"` // favicon, script, cookie
	Value    string `json:"value"`
	Others   int    `json:"others"`
	Programs int    `json:"programs"`
	// Generic is a value that links nothing: everywhere, or nowhere else.
	Generic bool   `json:"generic"`
	Query   string `json:"query"`
}

type techView struct {
	Name     string          `json:"name"`
	Version  string          `json:"version,omitempty"`
	Category string          `json:"category,omitempty"`
	CPE      *string         `json:"cpe,omitempty"`
	Proof    json.RawMessage `json:"proof,omitempty"`
}

type layerView struct {
	Layer          string     `json:"layer"`
	Informative    int32      `json:"informative_failures"`
	NonInformative int32      `json:"non_informative_failures"`
	FirstFailure   *time.Time `json:"first_failure_at"`
	LastOK         *time.Time `json:"last_ok_at"`
	LastChecked    time.Time  `json:"last_checked_at"`
}

func (s *server) getAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()
	q := s.Store.Q
	a, err := q.GetAsset(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "id not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	cur, err := q.GetAssetCurrent(ctx, id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	techRows, err := q.ListTech(ctx, id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	techs := make([]techView, 0, len(techRows))
	for _, t := range techRows {
		techs = append(techs, techView{Name: t.Name, Version: t.VersionRaw, Category: t.Category, CPE: t.Cpe, Proof: raw(t.Proof)})
	}
	layerRows, err := q.GetLayers(ctx, id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	layers := make([]layerView, 0, len(layerRows))
	for _, l := range layerRows {
		layers = append(layers, layerView{l.Layer, l.InformativeFailures, l.NonInformativeFailures, l.FirstFailureAt, l.LastOkAt, l.LastCheckedAt})
	}
	pivots, err := s.pivots(r, cur)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	children, err := q.ListChildren(ctx, &id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if children == nil {
		children = []sqlcgen.ListChildrenRow{}
	}
	if pivots == nil {
		pivots = []pivot{}
	}
	tr, err := q.GetTriage(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.internal(w, r, err)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		tr = sqlcgen.Triage{AssetID: id, State: "new", Tags: []string{}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asset": map[string]any{
			"id": a.ID, "program_id": a.ProgramID, "kind": a.Kind, "key": a.Key, "parent_id": a.ParentAssetID,
			"discovery_source": a.DiscoverySource, "lineage": raw(a.DiscoveryPath), "scope": a.ScopeStatus,
			"first_seen": a.FirstSeen, "last_seen": a.LastSeen,
		},
		"current":      cur,
		"technologies": techs,
		"layers":       layers,
		"pivots":       pivots,
		"children":     children,
		"triage":       tr,
	})
}

func (s *server) pivots(r *http.Request, cur sqlcgen.AssetCurrent) ([]pivot, error) {
	ctx := r.Context()
	q := s.Store.Q
	var out []pivot
	add := func(kind, value string, others, programs int32) {
		out = append(out, pivot{
			Kind: kind, Value: value, Others: int(others), Programs: int(programs),
			Generic: others == 0 || programs >= genericPrograms,
			Query:   search.Term{Field: kind, Values: []search.Value{{Lit: value}}}.String(),
		})
	}
	if cur.FaviconHash != nil {
		c, err := q.PivotFavicon(ctx, sqlcgen.PivotFaviconParams{Self: cur.AssetID, Value: *cur.FaviconHash})
		if err != nil {
			return nil, err
		}
		add("favicon", *cur.FaviconHash, c.Others, c.Programs)
	}
	for _, h := range cur.ScriptHashes {
		c, err := q.PivotScript(ctx, sqlcgen.PivotScriptParams{Self: cur.AssetID, Value: h})
		if err != nil {
			return nil, err
		}
		add("script", h, c.Others, c.Programs)
	}
	for _, n := range cur.CookieNames {
		c, err := q.PivotCookie(ctx, sqlcgen.PivotCookieParams{Self: cur.AssetID, Value: n})
		if err != nil {
			return nil, err
		}
		add("cookie", n, c.Others, c.Programs)
	}
	// The ones that link something first.
	slices.SortStableFunc(out, func(a, b pivot) int {
		if a.Generic != b.Generic {
			if a.Generic {
				return 1
			}
			return -1
		}
		return b.Others - a.Others
	})
	return out, nil
}

func (s *server) assetChanges(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.Store.Q.ListAssetChanges(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"kind": c.Kind, "layer": c.Layer, "before": raw(c.Before), "after": raw(c.After), "run_id": c.RunID, "at": c.At,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out})
}

var triageStates = []string{"new", "seen", "interesting", "ignored"}

func (s *server) patchTriage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State *string   `json:"state"`
		Tags  *[]string `json:"tags"`
		Note  *string   `json:"note"`
		Seen  bool      `json:"seen"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if body.State != nil && !slices.Contains(triageStates, *body.State) {
		writeError(w, http.StatusBadRequest, "bad_field", "state is new, seen, interesting or ignored")
		return
	}
	// One transaction with the row locked: two edits in flight (marking seen
	// while a tag is added) apply one after the other instead of one erasing
	// the other.
	s.inTx(w, r, func(q *sqlcgen.Queries) (int, any, error) {
		ctx := r.Context()
		if _, err := q.GetAsset(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "id not found")
			return 0, nil, errHandled
		} else if err != nil {
			return 0, nil, err
		}
		if err := q.EnsureTriage(ctx, id); err != nil {
			return 0, nil, err
		}
		cur, err := q.LockTriage(ctx, id)
		if err != nil {
			return 0, nil, err
		}
		if body.State != nil {
			cur.State = *body.State
		}
		if body.Tags != nil {
			tags := []string{}
			for _, t := range *body.Tags {
				if t = strings.ToLower(strings.TrimSpace(t)); t != "" && !slices.Contains(tags, t) {
					tags = append(tags, t)
				}
			}
			cur.Tags = tags
		}
		if body.Note != nil {
			cur.Note = body.Note
		}
		if body.Seen {
			now := s.now()
			cur.SeenAt = &now
			if cur.State == "new" {
				cur.State = "seen"
			}
		}
		row, err := q.PutTriage(ctx, sqlcgen.PutTriageParams{AssetID: id, State: cur.State, Tags: cur.Tags, Note: cur.Note, SeenAt: cur.SeenAt})
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, row, nil
	})
}
