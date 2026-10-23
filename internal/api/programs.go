package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

type programView struct {
	ID                uuid.UUID `json:"id"`
	Origin            string    `json:"origin"`
	Platform          *string   `json:"platform"`
	Slug              *string   `json:"slug"`
	Name              string    `json:"name"`
	Bounty            *bool     `json:"bounty"`
	State             string    `json:"state"`
	ArchivedBy        *string   `json:"archived_by"`
	AutoScan          *string   `json:"auto_scan"`
	EffectiveAutoScan string    `json:"effective_auto_scan"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	OpenRules         *int32    `json:"open_rules,omitempty"`
	Wildcards         *int32    `json:"wildcards,omitempty"`
}

func (s *server) viewProgram(p sqlcgen.Program) programView {
	eff := s.DefaultAutoScan
	if p.AutoScan != nil {
		eff = *p.AutoScan
	}
	return programView{
		ID: p.ID, Origin: p.Origin, Platform: p.Platform, Slug: p.Slug, Name: p.Name, Bounty: p.Bounty,
		State: p.State, ArchivedBy: p.ArchivedBy, AutoScan: p.AutoScan, EffectiveAutoScan: eff,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

type ruleView struct {
	ID        uuid.UUID  `json:"id"`
	Kind      string     `json:"kind"`
	Matcher   string     `json:"matcher"`
	Pattern   string     `json:"pattern"`
	Origin    string     `json:"origin"`
	Note      *string    `json:"note"`
	ValidFrom time.Time  `json:"valid_from"`
	ValidTo   *time.Time `json:"valid_to"`
	InForce   bool       `json:"in_force"`
}

func viewRule(r sqlcgen.ScopeRule) ruleView {
	return ruleView{
		ID: r.ID, Kind: r.Kind, Matcher: r.Matcher, Pattern: r.Pattern, Origin: r.Origin, Note: r.Note,
		ValidFrom: r.ValidFrom, ValidTo: r.ValidTo, InForce: r.ValidTo == nil,
	}
}

type wildcardView struct {
	ID          uuid.UUID  `json:"id"`
	ProgramID   uuid.UUID  `json:"program_id"`
	ProgramName string     `json:"program_name"`
	Platform    *string    `json:"platform"`
	Apex        string     `json:"apex"`
	Active      bool       `json:"active"`
	AutoScan    string     `json:"auto_scan"`
	LastReconAt *time.Time `json:"last_recon_at"`
	AssetsFound int32      `json:"assets_found"`
	CreatedAt   time.Time  `json:"created_at"`
}

func viewWildcard(w sqlcgen.ListWildcardsRow) wildcardView {
	return wildcardView{
		ID: w.ID, ProgramID: w.ProgramID, ProgramName: w.ProgramName, Platform: w.Platform, Apex: w.Apex,
		Active: w.Active, AutoScan: w.AutoScan, LastReconAt: w.LastReconAt, AssetsFound: w.AssetsFound,
		CreatedAt: w.CreatedAt,
	}
}

type untrackedView struct {
	Value  string    `json:"value"`
	Type   string    `json:"type"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

func (s *server) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

func (s *server) listPrograms(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.ListPrograms(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]programView, 0, len(rows))
	for _, row := range rows {
		v := s.viewProgram(sqlcgen.Program{
			ID: row.ID, Origin: row.Origin, Platform: row.Platform, Slug: row.Slug, Name: row.Name,
			Bounty: row.Bounty, State: row.State, ArchivedBy: row.ArchivedBy, AutoScan: row.AutoScan,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
		v.OpenRules, v.Wildcards = &row.OpenRules, &row.Wildcards
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"programs": out})
}

func validAutoScan(v *string) bool { return v == nil || *v == "off" || *v == "on_new" }

func (s *server) createProgram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string  `json:"name"`
		Platform *string `json:"platform"`
		AutoScan *string `json:"auto_scan"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_field", "name is required")
		return
	}
	if !validAutoScan(body.AutoScan) {
		writeError(w, http.StatusBadRequest, "bad_field", "auto_scan is off, on_new or null")
		return
	}
	if body.Platform != nil {
		p := strings.ToLower(strings.TrimSpace(*body.Platform))
		body.Platform = &p
	}
	p, err := s.Store.Q.CreateManualProgram(r.Context(), sqlcgen.CreateManualProgramParams{
		Name: body.Name, Platform: body.Platform, AutoScan: body.AutoScan, Now: s.now(),
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.viewProgram(p))
}

func (s *server) loadProgram(w http.ResponseWriter, r *http.Request, q *sqlcgen.Queries) (sqlcgen.Program, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return sqlcgen.Program{}, false
	}
	p, err := q.GetProgram(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "id not found")
		return sqlcgen.Program{}, false
	}
	if err != nil {
		s.internal(w, r, err)
		return sqlcgen.Program{}, false
	}
	return p, true
}

func (s *server) getProgram(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProgram(w, r, s.Store.Q)
	if !ok {
		return
	}
	ctx := r.Context()
	rules, err := s.Store.Q.ListRules(ctx, p.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	wildcards, err := s.Store.Q.ListWildcards(ctx, sqlcgen.ListWildcardsParams{
		DefaultAutoScan: s.DefaultAutoScan, ProgramID: &p.ID, IncludeInactive: true,
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	untracked, err := s.Store.Q.ListUntracked(ctx, p.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	res := struct {
		programView
		Rules     []ruleView      `json:"rules"`
		Wildcards []wildcardView  `json:"wildcards"`
		Untracked []untrackedView `json:"untracked"`
	}{programView: s.viewProgram(p), Rules: []ruleView{}, Wildcards: []wildcardView{}, Untracked: []untrackedView{}}
	for _, rl := range rules {
		res.Rules = append(res.Rules, viewRule(rl))
	}
	for _, wc := range wildcards {
		res.Wildcards = append(res.Wildcards, viewWildcard(wc))
	}
	for _, u := range untracked {
		res.Untracked = append(res.Untracked, untrackedView{u.Value, u.Type, u.Reason, u.FirstSeenAt})
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) patchProgram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     *string         `json:"name"`
		State    *string         `json:"state"`
		AutoScan json.RawMessage `json:"auto_scan"` // absent: unchanged, null: inherit
	}
	if !readJSON(w, r, &body) {
		return
	}
	p, ok := s.loadProgram(w, r, s.Store.Q)
	if !ok {
		return
	}
	params := sqlcgen.UpdateProgramParams{
		ID: p.ID, Name: p.Name, State: p.State, ArchivedBy: p.ArchivedBy, AutoScan: p.AutoScan, Now: s.now(),
	}
	if body.Name != nil {
		if p.Origin != "manual" {
			writeError(w, http.StatusConflict, "owned_by_sync", "a synced program takes its name from ScopesExtractor")
			return
		}
		if params.Name = strings.TrimSpace(*body.Name); params.Name == "" {
			writeError(w, http.StatusBadRequest, "bad_field", "name cannot be empty")
			return
		}
	}
	if body.State != nil && *body.State != p.State {
		switch *body.State {
		case "active", "paused":
			params.State, params.ArchivedBy = *body.State, nil
		case "archived":
			user := "user"
			params.State, params.ArchivedBy = "archived", &user
		default:
			writeError(w, http.StatusBadRequest, "bad_field", "state is active, paused or archived")
			return
		}
	}
	if body.AutoScan != nil {
		var v *string
		if err := json.Unmarshal(body.AutoScan, &v); err != nil || !validAutoScan(v) {
			writeError(w, http.StatusBadRequest, "bad_field", "auto_scan is off, on_new or null")
			return
		}
		params.AutoScan = v
	}
	updated, err := s.Store.Q.UpdateProgram(r.Context(), params)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.viewProgram(updated))
}

type ruleInput struct {
	Kind    *string `json:"kind"`
	Matcher *string `json:"matcher"`
	Pattern *string `json:"pattern"`
	Note    *string `json:"note"`
}

func (s *server) openRule(w http.ResponseWriter, r *http.Request) {
	var body ruleInput
	if !readJSON(w, r, &body) {
		return
	}
	rule, err := scope.ParseRule(deref(body.Kind), deref(body.Matcher), deref(body.Pattern))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_rule", err.Error())
		return
	}
	s.inTx(w, r, func(q *sqlcgen.Queries) (int, any, error) {
		p, ok := s.loadProgram(w, r, q)
		if !ok {
			return 0, nil, errHandled
		}
		row, err := scope.OpenRule(r.Context(), q, p.ID, rule, "local", body.Note, s.now())
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "rule_already_open", rule.String()+" is already in force")
			return 0, nil, errHandled
		}
		if err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, viewRule(row), nil
	})
}

func (s *server) patchRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ruleInput
		Close bool `json:"close"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ruleID, ok := pathID(w, r, "rule")
	if !ok {
		return
	}
	s.inTx(w, r, func(q *sqlcgen.Queries) (int, any, error) {
		p, ok := s.loadProgram(w, r, q)
		if !ok {
			return 0, nil, errHandled
		}
		old, err := q.GetRule(r.Context(), sqlcgen.GetRuleParams{ProgramID: p.ID, ID: ruleID})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "rule not found")
			return 0, nil, errHandled
		}
		if err != nil {
			return 0, nil, err
		}
		if old.Origin == "sync" {
			writeError(w, http.StatusConflict, "owned_by_sync", "the sync would put it back: add a local exclusion instead")
			return 0, nil, errHandled
		}
		if old.ValidTo != nil {
			writeError(w, http.StatusConflict, "rule_closed", "a closed rule is history")
			return 0, nil, errHandled
		}
		now := s.now()
		if body.Close {
			if body.Kind != nil || body.Matcher != nil || body.Pattern != nil || body.Note != nil {
				writeError(w, http.StatusBadRequest, "bad_body", "close takes no other field")
				return 0, nil, errHandled
			}
			if _, err := q.CloseRule(r.Context(), sqlcgen.CloseRuleParams{ID: old.ID, Now: now}); err != nil {
				return 0, nil, err
			}
			old.ValidTo = &now
			return http.StatusOK, viewRule(old), nil
		}
		next, err := scope.ParseRule(or(body.Kind, old.Kind), or(body.Matcher, old.Matcher), or(body.Pattern, old.Pattern))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_rule", err.Error())
			return 0, nil, errHandled
		}
		note := old.Note
		if body.Note != nil {
			note = body.Note
		}
		if next == (scope.Rule{Kind: old.Kind, Matcher: old.Matcher, Pattern: old.Pattern}) && deref(note) == deref(old.Note) {
			writeError(w, http.StatusBadRequest, "no_change", "the rule would be the same")
			return 0, nil, errHandled
		}
		// An edit is a close and an open, so the history holds both versions.
		if _, err := q.CloseRule(r.Context(), sqlcgen.CloseRuleParams{ID: old.ID, Now: now}); err != nil {
			return 0, nil, err
		}
		row, err := scope.OpenRule(r.Context(), q, p.ID, next, "local", note, now)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "rule_already_open", next.String()+" is already in force")
			return 0, nil, errHandled
		}
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, viewRule(row), nil
	})
}

func (s *server) listWildcards(w http.ResponseWriter, r *http.Request) {
	params := sqlcgen.ListWildcardsParams{
		DefaultAutoScan: s.DefaultAutoScan, IncludeInactive: r.URL.Query().Get("inactive") == "true",
	}
	if v := r.URL.Query().Get("program"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_field", "program is not an id")
			return
		}
		params.ProgramID = &id
	}
	rows, err := s.Store.Q.ListWildcards(r.Context(), params)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]wildcardView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewWildcard(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"wildcards": out})
}

func (s *server) runSync(w http.ResponseWriter, r *http.Request) {
	if s.Sync == nil {
		writeError(w, http.StatusServiceUnavailable, "sync_disabled", "no ScopesExtractor is configured")
		return
	}
	sum, err := s.Sync.Run(r.Context())
	switch {
	case errors.Is(err, scope.ErrRunning):
		writeError(w, http.StatusConflict, "sync_running", err.Error())
	case errors.Is(err, scope.ErrSuspicious):
		writeError(w, http.StatusConflict, "sync_refused", err.Error())
	case err != nil:
		writeError(w, http.StatusBadGateway, "sync_failed", err.Error())
	default:
		writeJSON(w, http.StatusOK, sum)
	}
}

// errHandled tells inTx the handler already answered.
var errHandled = errors.New("handled")

// inTx runs fn in a transaction and answers with what it returns. The
// transaction commits only when fn returns no error.
func (s *server) inTx(w http.ResponseWriter, r *http.Request, fn func(*sqlcgen.Queries) (int, any, error)) {
	tx, err := s.Store.Pool.Begin(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	code, v, err := fn(s.Store.Q.WithTx(tx))
	if errors.Is(err, errHandled) {
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, code, v)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func or(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
