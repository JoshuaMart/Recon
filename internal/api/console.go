package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// recentRuns is how far back the queue view looks.
//
// Enough to tell a stuck queue from an idle one, and no further. This is a
// present state and not a series, and a series needs a time series store, which
// is what the gauge is for.
const recentRuns = 20

// Console is the surface a person drives: perimeters, rules, and the queue.
//
// The inventory itself is not here. Reading the assets of a program is a search
// with a filter, and a second path to the same rows would be a second set of
// rules to keep in step.
type Console struct {
	db       *store.Scoped
	ingestor *ingest.Ingestor
	now      func() time.Time
	log      *slog.Logger
}

// NewConsole builds it.
func NewConsole(db *store.Scoped, ingestor *ingest.Ingestor, log *slog.Logger) *Console {
	return &Console{db: db, ingestor: ingestor, now: time.Now, log: log}
}

// program is one perimeter as a screen reads it.
type program struct {
	ID                uuid.UUID  `json:"id"`
	Name              string     `json:"name"`
	Platform          *string    `json:"platform,omitempty"`
	PlatformRef       *string    `json:"platform_ref,omitempty"`
	State             string     `json:"state"`
	AuthorizedFrom    time.Time  `json:"authorized_from"`
	AuthorizedTo      *time.Time `json:"authorized_to,omitempty"`
	AuthorizationRef  *string    `json:"authorization_ref,omitempty"`
	RateLimitRPS      int32      `json:"rate_limit_rps"`
	DiscoveryInterval string     `json:"discovery_interval"`
	LastDiscoveryAt   *time.Time `json:"last_discovery_at,omitempty"`
	// Version is what a write has to carry back. A stale one is a 409 rather
	// than a write.
	Version   int32     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// RulesInForce is on the list only, and only when asked for: it costs an
	// aggregation over the inventory and the switcher sits on every page. The
	// two counts are also answered by the detail, which is one program somebody
	// navigated to on purpose rather than a menu rendered everywhere.
	RulesInForce  *int32 `json:"rules_in_force,omitempty"`
	Assets        *int32 `json:"assets,omitempty"`
	AssetsInScope *int32 `json:"assets_in_scope,omitempty"`
}

// rule is one row of a perimeter.
type rule struct {
	ID        uuid.UUID  `json:"id"`
	Kind      string     `json:"kind"`
	Matcher   string     `json:"matcher"`
	Pattern   string     `json:"pattern"`
	ValidFrom time.Time  `json:"valid_from"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`
	Note      *string    `json:"note,omitempty"`
	Version   int32      `json:"version"`
	CreatedAt time.Time  `json:"created_at"`
	// InForce is answered by the server so no screen reimplements the window,
	// and so the comparison happens against the clock that wrote the values.
	InForce bool `json:"in_force"`
}

// effect is what a scope write moved, and it commits with the write.
//
// A write that says only "ok" leaves somebody running a search to find out what
// they just did.
type effect struct {
	Examined int `json:"examined"`
	Changed  int `json:"changed"`
	Gained   int `json:"gained"`
	Lost     int `json:"lost"`
}

// ListPrograms answers the switcher and the programs screen.
func (h *Console) ListPrograms(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()
	at := h.now()

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	rows, err := q.ListPrograms(ctx, sqlcgen.ListProgramsParams{
		OrgID: uuidTo(principal.OrgID), At: stamp(at),
	})
	if err != nil {
		h.unavailable(ctx, w, "list programmes failed", err)
		return
	}

	out := make([]program, 0, len(rows))
	for _, row := range rows {
		p := readProgram(row)
		rules := row.RulesInForce
		p.RulesInForce = &rules
		out = append(out, p)
	}

	// Asked for, not given. The default shape of this list is the one that
	// costs nothing, because the switcher renders it on every page.
	if r.URL.Query().Get("counts") == "1" {
		counts, err := q.CountProgramAssets(ctx, sqlcgen.CountProgramAssetsParams{
			OrgID: uuidTo(principal.OrgID),
		})
		if err != nil {
			h.unavailable(ctx, w, "count assets failed", err)
			return
		}
		byProgram := make(map[uuid.UUID]sqlcgen.CountProgramAssetsRow, len(counts))
		for _, row := range counts {
			byProgram[uuid.UUID(row.ProgramID.Bytes)] = row
		}
		for i := range out {
			// A programme with no asset is absent from the aggregation, and it
			// gets an explicit zero rather than no field: a missing counter and
			// a counter of zero read the same on a screen and mean different
			// things to the code that draws it.
			count := byProgram[out[i].ID]
			assets, inScope := count.Assets, count.InScope
			out[i].Assets, out[i].AssetsInScope = &assets, &inScope
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"programs": out})
}

// GetProgram answers one program and its rules.
func (h *Console) GetProgram(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()
	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}
	at := h.now()

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	row, err := q.GetProgram(ctx, sqlcgen.GetProgramParams{
		OrgID: uuidTo(principal.OrgID), ProgramID: uuidTo(programID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		h.missing(w)
		return
	}
	if err != nil {
		h.unavailable(ctx, w, "read programme failed", err)
		return
	}

	rules, err := q.ListRules(ctx, sqlcgen.ListRulesParams{
		OrgID: uuidTo(principal.OrgID), ProgramID: uuidTo(programID), At: stamp(at),
	})
	if err != nil {
		h.unavailable(ctx, w, "list rules failed", err)
		return
	}

	out := make([]rule, 0, len(rules))
	for _, row := range rules {
		out = append(out, readRule(row))
	}

	// The size of the perimeter, which is what this screen opens with. The same
	// statement the list runs, and it is grouped over the tenant rather than
	// filtered here because sqlc generates one function per statement and a
	// second spelling of the same aggregate is a second thing to keep in step.
	//
	// Zero rather than absent when the group by returned no row for this
	// program: a perimeter with nothing in it has a size, and it is nought. An
	// omitted field would read on the screen as a number nobody computed.
	counts, err := q.CountProgramAssets(ctx, sqlcgen.CountProgramAssetsParams{
		OrgID: uuidTo(principal.OrgID),
	})
	if err != nil {
		h.unavailable(ctx, w, "count programme assets failed", err)
		return
	}

	detail := readProgramOne(row)
	assets, inScope := int32(0), int32(0)
	for _, count := range counts {
		if uuid.UUID(count.ProgramID.Bytes) == programID {
			assets, inScope = count.Assets, count.InScope
			break
		}
	}
	detail.Assets, detail.AssetsInScope = &assets, &inScope

	writeJSON(w, http.StatusOK, map[string]any{
		"program": detail, "rules": out,
	})
}

// programBody is what a create or an edit carries.
type programBody struct {
	Name              string     `json:"name"`
	Platform          *string    `json:"platform"`
	PlatformRef       *string    `json:"platform_ref"`
	State             string     `json:"state"`
	AuthorizedFrom    *time.Time `json:"authorized_from"`
	AuthorizedTo      *time.Time `json:"authorized_to"`
	AuthorizationRef  *string    `json:"authorization_ref"`
	RateLimitRPS      int32      `json:"rate_limit_rps"`
	DiscoveryInterval string     `json:"discovery_interval"`
	// Version is required on an edit and meaningless on a create, where there
	// is nothing to avoid overwriting.
	Version *int32 `json:"version"`
}

// CreateProgram opens a perimeter.
func (h *Console) CreateProgram(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	body, ok := read[programBody](w, r)
	if !ok {
		return
	}
	if body.Name == "" {
		fail(w, http.StatusBadRequest, "no_name", "a programme needs a name")
		return
	}
	if body.State == "" {
		body.State = "active"
	}
	if body.RateLimitRPS <= 0 {
		body.RateLimitRPS = 10
	}
	interval, err := parseInterval(body.DiscoveryInterval, 7*24*time.Hour)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_interval", err.Error())
		return
	}
	from := h.now()
	if body.AuthorizedFrom != nil {
		from = *body.AuthorizedFrom
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := sqlcgen.New(tx).CreateProgram(ctx, sqlcgen.CreateProgramParams{
		ProgramID:         uuidTo(uuid.New()),
		OrgID:             uuidTo(principal.OrgID),
		Name:              body.Name,
		Platform:          body.Platform,
		PlatformRef:       body.PlatformRef,
		AuthorizedFrom:    stamp(from),
		AuthorizedTo:      stampMaybe(body.AuthorizedTo),
		AuthorizationRef:  body.AuthorizationRef,
		RateLimitRps:      body.RateLimitRPS,
		DiscoveryInterval: interval,
		State:             body.State,
		Actor:             actor(principal),
	})
	if err != nil {
		h.refuseOrFail(ctx, w, "create programme failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"program": readProgramCreated(row)})
}

// UpdateProgram edits one, and reclassifies with it.
//
// Suspending a programme does not change the perimeter, and the pass runs
// anyway. It is what carries the due dates, and a pass that only ran on rule
// writes would be a rule nobody wrote deciding when the inventory is correct.
func (h *Console) UpdateProgram(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()
	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}
	body, ok := read[programBody](w, r)
	if !ok {
		return
	}
	if body.Version == nil {
		fail(w, http.StatusBadRequest, "no_version",
			"an edit carries the version it read, so a write cannot silently overwrite another")
		return
	}
	if body.Name == "" {
		fail(w, http.StatusBadRequest, "no_name", "a programme needs a name")
		return
	}
	if body.State == "" {
		body.State = "active"
	}
	if body.RateLimitRPS <= 0 {
		body.RateLimitRPS = 10
	}
	interval, err := parseInterval(body.DiscoveryInterval, 7*24*time.Hour)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_interval", err.Error())
		return
	}
	at := h.now()
	from := at
	if body.AuthorizedFrom != nil {
		from = *body.AuthorizedFrom
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	row, err := q.UpdateProgram(ctx, sqlcgen.UpdateProgramParams{
		OrgID:             uuidTo(principal.OrgID),
		ProgramID:         uuidTo(programID),
		Version:           *body.Version,
		Name:              body.Name,
		Platform:          body.Platform,
		PlatformRef:       body.PlatformRef,
		AuthorizedFrom:    stamp(from),
		AuthorizedTo:      stampMaybe(body.AuthorizedTo),
		AuthorizationRef:  body.AuthorizationRef,
		RateLimitRps:      body.RateLimitRPS,
		DiscoveryInterval: interval,
		State:             body.State,
		Actor:             actor(principal),
		At:                stamp(at),
	})
	// No row means either no such programme or a version that moved, and the
	// two are told apart by asking. That second read is the only way to answer
	// 409 rather than 404 on a stale write, and answering 404 there would send
	// somebody looking for a programme that is right in front of them.
	if errors.Is(err, pgx.ErrNoRows) {
		h.stale(ctx, w, q, principal, programID)
		return
	}
	if err != nil {
		h.refuseOrFail(ctx, w, "update programme failed", err)
		return
	}

	set, err := compileScope(ctx, q, programID, at)
	if err != nil {
		h.unavailable(ctx, w, "read perimeter failed", err)
		return
	}
	moved, err := h.reclassify(ctx, q, programID, at, set)
	if err != nil {
		h.unavailable(ctx, w, "reclassify failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"program": readProgramUpdated(row), "effect": moved})
}

// ruleBody is what a rule write carries.
type ruleBody struct {
	Kind      string     `json:"kind"`
	Matcher   string     `json:"matcher"`
	Pattern   string     `json:"pattern"`
	ValidFrom *time.Time `json:"valid_from"`
	ValidTo   *time.Time `json:"valid_to"`
	Note      *string    `json:"note"`
	Version   *int32     `json:"version"`
}

// CreateRule opens a rule and reclassifies in the same transaction.
func (h *Console) CreateRule(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()
	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}
	body, ok := read[ruleBody](w, r)
	if !ok {
		return
	}
	if err := validRule(body); err != nil {
		fail(w, http.StatusBadRequest, "bad_rule", err.Error())
		return
	}
	at := h.now()
	from := at
	if body.ValidFrom != nil {
		from = *body.ValidFrom
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if !h.owns(ctx, w, q, principal, programID) {
		return
	}

	row, err := q.CreateRule(ctx, sqlcgen.CreateRuleParams{
		RuleID:    uuidTo(uuid.New()),
		OrgID:     uuidTo(principal.OrgID),
		ProgramID: uuidTo(programID),
		Kind:      body.Kind,
		Matcher:   body.Matcher,
		Pattern:   body.Pattern,
		ValidFrom: stamp(from),
		ValidTo:   stampMaybe(body.ValidTo),
		Note:      body.Note,
		Actor:     actor(principal),
	})
	if err != nil {
		h.refuseOrFail(ctx, w, "create rule failed", err)
		return
	}

	// The perimeter as it stands with the new rule in it, compiled once and
	// used twice. Reading the rules back rather than taking the one just
	// written, because classification is the whole set: an include added beside
	// an exclude has to be evaluated with it.
	set, err := compileScope(ctx, q, programID, at)
	if err != nil {
		if errors.Is(err, scope.ErrInvalidRule) {
			fail(w, http.StatusBadRequest, "bad_rule", err.Error())
			return
		}
		h.unavailable(ctx, w, "read perimeter failed", err)
		return
	}

	// An include that names one thing declares it as well as classifying it.
	//
	// An apex says where enumeration starts and a run finds what is under it.
	// The other two name something that exists: a host to probe, or a path to
	// render. Without this they were classifiers with nothing to classify, and
	// the rule read as in force over an inventory it had never put anything
	// into.
	seeded, err := h.seed(ctx, q, principal, programID, set, body)
	if err != nil {
		var bad *refusedPattern
		if errors.As(err, &bad) {
			fail(w, http.StatusBadRequest, "bad_rule", bad.reason)
			return
		}
		h.unavailable(ctx, w, "seed the rule failed", err)
		return
	}

	// After the write and inside the same transaction, which is the whole
	// point: a rule in force whose consequence the inventory does not carry is
	// a perimeter that lies, and the window between two transactions is a
	// window where the system scans what was just taken away from it.
	moved, err := h.reclassify(ctx, q, programID, at, set)
	if err != nil {
		h.unavailable(ctx, w, "reclassify failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"rule": readRuleCreated(row, at), "effect": moved, "declared": seeded,
	})
}

// UpdateRule edits or closes one.
//
// Closing is setting valid_to. There is no delete on this surface at all: a
// rule has a period of validity rather than an existence, and an asset
// classified by a rule since closed stays explainable.
func (h *Console) UpdateRule(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()
	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}
	ruleID, ok := pathUUID(w, r, "rule")
	if !ok {
		return
	}
	body, ok := read[ruleBody](w, r)
	if !ok {
		return
	}
	if body.Version == nil {
		fail(w, http.StatusBadRequest, "no_version",
			"an edit carries the version it read, so a write cannot silently overwrite another")
		return
	}
	at := h.now()

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if !h.owns(ctx, w, q, principal, programID) {
		return
	}

	row, err := q.UpdateRule(ctx, sqlcgen.UpdateRuleParams{
		OrgID:     uuidTo(principal.OrgID),
		ProgramID: uuidTo(programID),
		RuleID:    uuidTo(ruleID),
		Version:   *body.Version,
		// Absent leaves the pattern alone, which is what closing a rule does.
		Pattern: optional(body.Pattern),
		Note:    body.Note,
		ValidTo: stampMaybe(body.ValidTo),
		Actor:   actor(principal),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The programme is already known to be this caller's, so two readings are
		// left: a version that moved, and a rule this programme does not have.
		// The answer names both rather than picking one, because separating them
		// costs a second read and the action is the same either way: reread the
		// programme before writing again.
		fail(w, http.StatusConflict, "stale_version",
			"the rule moved since it was read, or this programme has no such rule. "+
				"Nothing was written.")
		return
	}
	if err != nil {
		h.refuseOrFail(ctx, w, "update rule failed", err)
		return
	}

	// Nothing is seeded here, and the asymmetry is deliberate. Closing a rule
	// must not create anything, and this surface offers no way to change a
	// pattern, so the only edit that reaches it is the close.
	set, err := compileScope(ctx, q, programID, at)
	if err != nil {
		if errors.Is(err, scope.ErrInvalidRule) {
			fail(w, http.StatusBadRequest, "bad_rule", err.Error())
			return
		}
		h.unavailable(ctx, w, "read perimeter failed", err)
		return
	}

	moved, err := h.reclassify(ctx, q, programID, at, set)
	if err != nil {
		h.unavailable(ctx, w, "reclassify failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule": readRuleUpdated(row, at), "effect": moved})
}

// Queue answers "why is nothing moving".
//
// Read from a console and never from what consumes it. The depth gauge exists
// for the metrics store; this is the question somebody asks in front of a
// screen, and answering it should not require a psql session.
func (h *Console) Queue(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()
	at := h.now()

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	depths, err := q.QueueDepth(ctx, sqlcgen.QueueDepthParams{
		OrgID: uuidTo(principal.OrgID), At: stamp(at),
	})
	if err != nil {
		h.unavailable(ctx, w, "read queue failed", err)
		return
	}
	runs, err := q.RecentRuns(ctx, sqlcgen.RecentRunsParams{
		OrgID: uuidTo(principal.OrgID), Batch: recentRuns,
	})
	if err != nil {
		h.unavailable(ctx, w, "read runs failed", err)
		return
	}

	type depth struct {
		ProgramID uuid.UUID `json:"program_id"`
		Queue     string    `json:"queue"`
		Due       int32     `json:"due"`
		Later     int32     `json:"later"`
		InRun     int32     `json:"in_run"`
	}
	outDepths := make([]depth, 0, len(depths))
	for _, row := range depths {
		// A pair whose three numbers are zero is dropped, so an entry here is
		// always a queue that exists.
		if row.Due == 0 && row.Later == 0 && row.InRun == 0 {
			continue
		}
		outDepths = append(outDepths, depth{
			ProgramID: uuid.UUID(row.ProgramID.Bytes), Queue: row.Queue,
			Due: row.Due, Later: row.Later, InRun: row.InRun,
		})
	}

	type run struct {
		ID           uuid.UUID  `json:"id"`
		ProgramID    uuid.UUID  `json:"program_id"`
		Kind         string     `json:"kind"`
		Scope        string     `json:"scope"`
		State        string     `json:"state"`
		Deadline     time.Time  `json:"deadline"`
		CreatedAt    time.Time  `json:"created_at"`
		StartedAt    *time.Time `json:"started_at,omitempty"`
		FinishedAt   *time.Time `json:"finished_at,omitempty"`
		TargetCount  *int32     `json:"target_count,omitempty"`
		Observations int32      `json:"observations"`
		Error        *string    `json:"error,omitempty"`
		// ExternalID is what the platform called the execution. Absent means
		// nothing started it, which is a different thing from a run nothing has
		// opened yet, and the two want opposite actions.
		ExternalID *string `json:"external_id,omitempty"`
	}
	outRuns := make([]run, 0, len(runs))
	for _, row := range runs {
		outRuns = append(outRuns, run{
			ID:        uuid.UUID(row.ID.Bytes),
			ProgramID: uuid.UUID(row.ProgramID.Bytes),
			Kind:      row.Kind, Scope: row.Scope, State: row.State,
			Deadline:  row.Deadline.Time,
			CreatedAt: row.CreatedAt.Time,
			// started_at is what separates a run something opened from one
			// whose provisioning failed, and those two call for opposite
			// actions.
			StartedAt:    instant(row.StartedAt),
			FinishedAt:   instant(row.FinishedAt),
			TargetCount:  row.TargetCount,
			Observations: row.Observations,
			Error:        row.Error,
			ExternalID:   row.ExternalID,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"depths": outDepths, "runs": outRuns})
}

// Coverage answers what Certificate Transparency has actually delivered under a
// programme's apexes.
//
// It returns numbers and the span they cover, and no score. A coverage
// confidence collapsed into one figure is the composite score this console is
// built without, and the argument is the same one: a reader can tell "watched a
// month, no certificate" from "watched since this morning", and a single number
// cannot. What is stated instead is every input somebody would have used to
// compute one.
//
// The feed's uptime travels beside the counters because without it an apex the
// logs are silent about and a socket that was down read identically, and the
// second is this deployment's problem rather than a fact about the logs.
func (h *Console) Coverage(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()
	at := h.now()

	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if !h.owns(ctx, w, q, principal, programID) {
		return
	}

	rows, err := q.ApexCoverage(ctx, sqlcgen.ApexCoverageParams{ProgramID: uuidTo(programID)})
	if err != nil {
		h.unavailable(ctx, w, "read coverage failed", err)
		return
	}

	type apex struct {
		Apex         string     `json:"apex"`
		WatchedSince time.Time  `json:"watched_since"`
		Names        int64      `json:"names"`
		Wildcards    int64      `json:"wildcards"`
		Dropped      int64      `json:"dropped"`
		LastName     *time.Time `json:"last_name_at,omitempty"`
		LastWildcard *time.Time `json:"last_wildcard_at,omitempty"`
		// FeedMinutes is how many minutes the feed delivered anything since
		// this apex was first watched, against WatchedMinutes. Two numbers
		// rather than a ratio, for the reason the whole endpoint gives.
		FeedMinutes    int64 `json:"feed_minutes"`
		WatchedMinutes int64 `json:"watched_minutes"`
	}

	out := make([]apex, 0, len(rows))
	for _, row := range rows {
		uptime, err := q.FeedUptime(ctx, sqlcgen.FeedUptimeParams{Since: row.WatchedSince})
		if err != nil {
			h.unavailable(ctx, w, "read feed uptime failed", err)
			return
		}
		watched := int64(at.Sub(row.WatchedSince.Time) / time.Minute)
		if watched < 0 {
			watched = 0
		}
		out = append(out, apex{
			Apex:           row.Apex,
			WatchedSince:   row.WatchedSince.Time,
			Names:          row.SanCount,
			Wildcards:      row.WildcardCount,
			Dropped:        row.Dropped,
			LastName:       instant(row.LastSanAt),
			LastWildcard:   instant(row.LastWildcardAt),
			FeedMinutes:    uptime.Minutes,
			WatchedMinutes: watched,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"apexes": out})
}

// reclassify re-evaluates the programme against the perimeter it is given.
//
// The set is compiled by the caller and passed in, because the two things that
// happen on a rule write need the same one: what a rule declares has to be
// classified by the perimeter that now includes it, and compiling twice would
// be two reads of a table that cannot have changed between them.
func (h *Console) reclassify(
	ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID, at time.Time, set *scope.Set,
) (effect, error) {
	moved, err := h.ingestor.Reclassify(ctx, q, programID, set, ingest.DefaultSchedule(at, false))
	if err != nil {
		return effect{}, err
	}
	return effect{
		Examined: moved.Examined, Changed: moved.Moved,
		Gained: moved.Gained, Lost: moved.Lost,
	}, nil
}

// refusedPattern is a pattern the entry path could not read, told apart from a
// failure of ours so a typo does not look like an outage.
type refusedPattern struct{ reason string }

func (r *refusedPattern) Error() string { return r.reason }

// seed creates what an include names, when the matcher names something.
//
// `fqdn` and `url_prefix` are the two that point at a thing rather than at a
// shape: one is a host to probe, the other a path to render. An `apex` names
// where to start looking and a run finds what is under it, a `cidr` names a
// range nobody can enumerate from, and a `regex` names a shape rather than a
// thing, so none of the three declares anything.
//
// The same path the assets form uses, because it is the same act: it creates the
// chain a URL needs, gives the host the due date, and lets the declared path earn
// its render once the service has answered. The lineage says which of the two
// acts it was.
//
// Exclusions never seed. Naming something to take it out of a perimeter is not a
// reason to put it in one.
func (h *Console) seed(
	ctx context.Context, q *sqlcgen.Queries, principal auth.Principal,
	programID uuid.UUID, set *scope.Set, body ruleBody,
) ([]ingest.Accepted, error) {
	if body.Kind != scope.Include {
		return nil, nil
	}
	switch body.Matcher {
	case scope.MatchFQDN, scope.MatchURLPrefix:
	default:
		return nil, nil
	}

	entered, err := h.ingestor.Enter(ctx, q, ingest.Run{
		ID:        uuid.New(),
		OrgID:     principal.OrgID,
		ProgramID: programID,
		Kind:      "manual",
		Source:    ingest.SourceRule,
	}, set, []string{body.Pattern})
	if err != nil {
		return nil, err
	}
	// A pattern the entry path cannot represent is refused rather than stored.
	// A rule that names something this system has no way to hold is a rule that
	// will not do what it says, and the refusal reaches somebody looking at the
	// form.
	if len(entered.Refused) > 0 {
		return nil, &refusedPattern{reason: entered.Refused[0].Reason}
	}
	return entered.Accepted, nil
}

// stale decides between 404 and 409 on a write that changed nothing.
func (h *Console) stale(
	ctx context.Context, w http.ResponseWriter, q *sqlcgen.Queries,
	principal auth.Principal, programID uuid.UUID,
) {
	_, err := q.GetProgram(ctx, sqlcgen.GetProgramParams{
		OrgID: uuidTo(principal.OrgID), ProgramID: uuidTo(programID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		h.missing(w)
		return
	}
	if err != nil {
		h.unavailable(ctx, w, "read programme failed", err)
		return
	}
	// The refusal is not a syntax error. The caller based a decision on a state
	// that no longer exists, and the only honest answer is to say so, so it can
	// reread before rewriting.
	fail(w, http.StatusConflict, "stale_version",
		"the programme moved since it was read, so nothing was written")
}

// owns refuses a programme that is not this caller's, as a 404.
func (h *Console) owns(
	ctx context.Context, w http.ResponseWriter, q *sqlcgen.Queries,
	principal auth.Principal, programID uuid.UUID,
) bool {
	_, err := q.GetProgram(ctx, sqlcgen.GetProgramParams{
		OrgID: uuidTo(principal.OrgID), ProgramID: uuidTo(programID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		h.missing(w)
		return false
	}
	if err != nil {
		h.unavailable(ctx, w, "read programme failed", err)
		return false
	}
	return true
}

func (h *Console) missing(w http.ResponseWriter) {
	fail(w, http.StatusNotFound, "not_found", "no such programme")
}

func (h *Console) unavailable(ctx context.Context, w http.ResponseWriter, message string, err error) {
	h.log.ErrorContext(ctx, message, "error", err)
	fail(w, http.StatusInternalServerError, "unavailable", "the request could not be served")
}

// refuseOrFail tells a constraint the caller broke from a failure that is ours.
//
// The model carries named checks on the state, the kind, the matcher and the
// authorization window, and every one of them describes an input somebody
// typed. Answering 500 to those would make the screen unusable while the fault
// is entirely on the form.
func (h *Console) refuseOrFail(ctx context.Context, w http.ResponseWriter, message string, err error) {
	var violation *pgconn.PgError
	// Class 23 is integrity: a check, a foreign key, a not-null. Everything
	// else in that catalogue is ours, and answering 400 to a broken connection
	// would tell somebody to fix their form.
	if errors.As(err, &violation) && strings.HasPrefix(violation.Code, "23") {
		fail(w, http.StatusBadRequest, "refused", violation.Message)
		return
	}
	h.unavailable(ctx, w, message, err)
}

// read decodes a body of a bounded size.
func read[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var body T
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "malformed", "the body is not what this route takes")
		return body, false
	}
	return body, true
}

// validRule checks what the database would refuse anyway, so the message names
// the field rather than the constraint.
func validRule(body ruleBody) error {
	switch body.Kind {
	case scope.Include, scope.Exclude:
	default:
		return fmt.Errorf("kind %q is neither include nor exclude", body.Kind)
	}
	switch body.Matcher {
	case scope.MatchApex, scope.MatchFQDN, scope.MatchCIDR, scope.MatchRegex, scope.MatchURLPrefix:
	default:
		return fmt.Errorf("matcher %q is not one this system knows", body.Matcher)
	}
	if body.Pattern == "" {
		return errors.New("a rule needs a pattern")
	}
	// Compiled here so a pattern that cannot compile is refused before it is
	// written. A rule sitting in the table that the perimeter cannot read makes
	// every later reclassification fail, and the write that broke it is long
	// gone by then.
	if _, err := scope.Compile([]scope.Rule{{
		Kind: body.Kind, Matcher: body.Matcher, Pattern: body.Pattern,
	}}); err != nil {
		return err
	}
	// And refused if it compiles into something that can never match. That is
	// the worse failure of the two: a rule that will not compile announces
	// itself, and one that matches nothing reads as in force while the whole
	// perimeter it was meant to cover stays unknown and unprobed.
	return scope.Unmatchable(body.Matcher, body.Pattern)
}

// parseInterval reads a discovery cadence.
func parseInterval(value string, fallback time.Duration) (pgtype.Interval, error) {
	if value == "" {
		return interval(fallback), nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return pgtype.Interval{}, fmt.Errorf("discovery_interval %q is not a duration", value)
	}
	if parsed <= 0 {
		return pgtype.Interval{}, errors.New("discovery_interval must be positive")
	}
	return interval(parsed), nil
}

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// actor is who is writing, and it is null while the credential belongs to the
// organization rather than to a person.
//
// Said here rather than left implicit: an attribution column believed to be
// populated is worse than an absent one.
func actor(principal auth.Principal) pgtype.UUID {
	if principal.ActorID == nil {
		return pgtype.UUID{}
	}
	return uuidTo(*principal.ActorID)
}

// optional turns an omitted string into an absent value, so a field nobody sent
// is a field nobody meant to change.
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stampMaybe(at *time.Time) pgtype.Timestamptz {
	if at == nil {
		return pgtype.Timestamptz{}
	}
	return stamp(*at)
}

func instant(at pgtype.Timestamptz) *time.Time {
	if !at.Valid {
		return nil
	}
	value := at.Time
	return &value
}

// The four program statements return the same columns on purpose, and these
// are the four adapters that say so. sqlc generates a distinct type per
// statement, so one shape for one object costs four functions here; three
// hand-written shapes would cost a field forgotten in one of them.

func readProgram(row sqlcgen.ListProgramsRow) program {
	return program{
		ID: uuid.UUID(row.ID.Bytes), Name: row.Name,
		Platform: row.Platform, PlatformRef: row.PlatformRef, State: row.State,
		AuthorizedFrom: row.AuthorizedFrom.Time, AuthorizedTo: instant(row.AuthorizedTo),
		AuthorizationRef: row.AuthorizationRef, RateLimitRPS: row.RateLimitRps,
		DiscoveryInterval: readInterval(row.DiscoveryInterval),
		LastDiscoveryAt:   instant(row.LastDiscoveryAt),
		Version:           row.Version,
		CreatedAt:         row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func readProgramOne(row sqlcgen.GetProgramRow) program {
	return readProgram(sqlcgen.ListProgramsRow(sqlcgen.ListProgramsRow{
		ID: row.ID, Name: row.Name, Platform: row.Platform, PlatformRef: row.PlatformRef,
		State: row.State, AuthorizedFrom: row.AuthorizedFrom, AuthorizedTo: row.AuthorizedTo,
		AuthorizationRef: row.AuthorizationRef, RateLimitRps: row.RateLimitRps,
		DiscoveryInterval: row.DiscoveryInterval, LastDiscoveryAt: row.LastDiscoveryAt,
		Version: row.Version, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}))
}

func readProgramCreated(row sqlcgen.CreateProgramRow) program {
	return readProgram(sqlcgen.ListProgramsRow{
		ID: row.ID, Name: row.Name, Platform: row.Platform, PlatformRef: row.PlatformRef,
		State: row.State, AuthorizedFrom: row.AuthorizedFrom, AuthorizedTo: row.AuthorizedTo,
		AuthorizationRef: row.AuthorizationRef, RateLimitRps: row.RateLimitRps,
		DiscoveryInterval: row.DiscoveryInterval, LastDiscoveryAt: row.LastDiscoveryAt,
		Version: row.Version, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	})
}

func readProgramUpdated(row sqlcgen.UpdateProgramRow) program {
	return readProgram(sqlcgen.ListProgramsRow{
		ID: row.ID, Name: row.Name, Platform: row.Platform, PlatformRef: row.PlatformRef,
		State: row.State, AuthorizedFrom: row.AuthorizedFrom, AuthorizedTo: row.AuthorizedTo,
		AuthorizationRef: row.AuthorizationRef, RateLimitRps: row.RateLimitRps,
		DiscoveryInterval: row.DiscoveryInterval, LastDiscoveryAt: row.LastDiscoveryAt,
		Version: row.Version, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	})
}

func readRule(row sqlcgen.ListRulesRow) rule {
	out := rule{
		ID: uuid.UUID(row.ID.Bytes), Kind: row.Kind, Matcher: row.Matcher,
		Pattern: row.Pattern, ValidFrom: row.ValidFrom.Time, ValidTo: instant(row.ValidTo),
		Note: row.Note, Version: row.Version, CreatedAt: row.CreatedAt.Time,
	}
	if row.InForce != nil {
		out.InForce = *row.InForce
	}
	return out
}

func readRuleCreated(row sqlcgen.CreateRuleRow, at time.Time) rule {
	return rule{
		ID: uuid.UUID(row.ID.Bytes), Kind: row.Kind, Matcher: row.Matcher,
		Pattern: row.Pattern, ValidFrom: row.ValidFrom.Time, ValidTo: instant(row.ValidTo),
		Note: row.Note, Version: row.Version, CreatedAt: row.CreatedAt.Time,
		InForce: inForce(row.ValidFrom, row.ValidTo, at),
	}
}

func readRuleUpdated(row sqlcgen.UpdateRuleRow, at time.Time) rule {
	return rule{
		ID: uuid.UUID(row.ID.Bytes), Kind: row.Kind, Matcher: row.Matcher,
		Pattern: row.Pattern, ValidFrom: row.ValidFrom.Time, ValidTo: instant(row.ValidTo),
		Note: row.Note, Version: row.Version, CreatedAt: row.CreatedAt.Time,
		InForce: inForce(row.ValidFrom, row.ValidTo, at),
	}
}

// inForce answers the window against the instant the write used, which is the
// clock that wrote the values being compared. Reaching for now() here would put
// the application's clock on one side and the database's on the other.
func inForce(from, to pgtype.Timestamptz, at time.Time) bool {
	if from.Valid && from.Time.After(at) {
		return false
	}
	return !to.Valid || to.Time.After(at)
}

// readInterval renders a cadence as the duration string the form takes back.
//
// Months and days are carried as their own components, and a cadence expressed
// in either would come back as "0s" if only the microseconds were read. Nothing
// writes one today; reading all three is what keeps that true when something
// does.
func readInterval(value pgtype.Interval) string {
	if !value.Valid {
		return ""
	}
	total := time.Duration(value.Microseconds) * time.Microsecond
	total += time.Duration(value.Days) * 24 * time.Hour
	total += time.Duration(value.Months) * 30 * 24 * time.Hour
	return total.String()
}
