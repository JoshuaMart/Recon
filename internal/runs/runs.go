// Package runs decides what to scan next, freezes it, and hands out the
// definition an execution needs.
//
// There is no queue table and no lease column. Three due dates decide
// eligibility, and the frozen target list of a run is the whole of the
// reservation: selection skips what a live run already holds, and the
// reservation expires when the run does. A run that dies takes nothing with it,
// because due dates move only when a report is ingested.
package runs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/config"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Kinds of run. The difference is the whole of the two mandates: one is given a
// domain and allowed to find things, the other is given a list and is the only
// shape in which a missing answer means anything.
const (
	KindDiscovery    = "discovery"
	KindVerification = "verification"
)

// Errors a caller acts on.
var (
	// ErrNothingDue is not a failure. A tick that finds nothing to do is the
	// normal state of a healthy inventory.
	ErrNothingDue = errors.New("nothing is due")
	// ErrRunInFlight names a run somebody has to decide about, which is why it
	// carries the run rather than a sentence.
	ErrRunInFlight = errors.New("a run is already in flight")
	// ErrNoPerimeter is a programme with no apex to enumerate.
	ErrNoPerimeter = errors.New("the programme declares no apex")
)

// InFlight is what a refusal has to say.
//
// Two situations call for opposite actions here: a run something has actually
// opened is a run to wait for, and a run nobody has claimed is a run whose
// provisioning failed. What separates them is StartedAt, written by the first
// report, which is the only thing that says a scanner opened the run rather
// than a provisioner having promised to.
type InFlight struct {
	ID        uuid.UUID
	Kind      string
	Scope     string
	State     string
	CreatedAt time.Time
	StartedAt *time.Time
	Deadline  time.Time
	Targets   int
}

// Age is how long the run has been around, which is the number a person reads
// before deciding.
func (f InFlight) Age(now time.Time) time.Duration { return now.Sub(f.CreatedAt) }

// Claimed reports whether a scanner ever opened it.
func (f InFlight) Claimed() bool { return f.StartedAt != nil }

func (f InFlight) Error() string {
	claimed := "nothing has opened it"
	if f.Claimed() {
		claimed = "a scanner opened it at " + f.StartedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("run %s (%s, %s) is %s and %s", f.ID, f.Kind, f.Scope, f.State, claimed)
}

// Definition is everything an execution receives.
//
// Nothing it holds opens the inventory. The two credentials are HMACs over the
// run, the purpose and an expiry, so there is nothing to store, nothing to
// revoke and nothing to purge.
type Definition struct {
	RunID       uuid.UUID
	OrgID       uuid.UUID
	ProgramID   uuid.UUID
	Kind        string
	Scope       string
	Deadline    time.Time
	TargetCount int
	// Env is the environment map the job definition is started with. It is a
	// full replacement of the run's own overrides and never a merge into the
	// definition: the definition carries the source API keys, and a control
	// plane that wrote into it would wipe them without anything failing.
	Env map[string]string
}

// Scheduler provisions runs.
type Scheduler struct {
	signer *auth.Signer
	cfg    config.Verification
	now    func() time.Time
	log    *slog.Logger
}

// Option adjusts a scheduler.
type Option func(*Scheduler)

// WithClock replaces the instant deadlines are measured from. A sweeper that
// can only be tested by waiting is a sweeper nobody tests.
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// New builds a scheduler.
func New(signer *auth.Signer, cfg config.Verification, log *slog.Logger, opts ...Option) *Scheduler {
	scheduler := &Scheduler{signer: signer, cfg: cfg, now: time.Now, log: log}
	for _, opt := range opts {
		opt(scheduler)
	}
	return scheduler
}

// Cadence is what the ingestor reschedules with, so both read one setting.
func (s *Scheduler) Cadence() lifecycle.Cadence {
	return lifecycle.Cadence{
		Resolve:     s.cfg.Resolve,
		Full:        s.cfg.Full,
		Fingerprint: s.cfg.Fingerprint,
		Inactive:    s.cfg.Inactive,
		Jitter:      s.cfg.Jitter,
		FullFloor:   s.cfg.FullFloor,
	}
}

// Verification freezes what is due on one rung and returns its definition.
//
// It runs in a transaction with the selection, which is what makes the frozen
// list a lease at all: between reading the due assets and writing the target
// rows, another tick reading the same rows would hand the same hosts to two
// runs.
func (s *Scheduler) Verification(
	ctx context.Context, q *sqlcgen.Queries, org, program uuid.UUID, rung string,
) (*Definition, error) {
	if rung != lifecycle.RungResolve && rung != lifecycle.RungFull {
		return nil, fmt.Errorf("unknown rung %q", rung)
	}

	// Asked before anything is selected. A programme whose run is in flight has
	// nothing due by construction, since the frozen list is what selection
	// skips, and answering "nothing is due" there would hide the run somebody
	// actually has to decide about.
	if live, err := s.InFlight(ctx, q, program, KindVerification); err != nil {
		return nil, err
	} else if live != nil {
		return nil, fmt.Errorf("%w: %s", ErrRunInFlight, live.Error())
	}

	at := s.now()
	due, err := q.SelectDueHosts(ctx, sqlcgen.SelectDueHostsParams{
		OrgID:     pgUUID(org),
		ProgramID: pgUUID(program),
		Rung:      rung,
		At:        stamp(at),
		Batch:     bounded(s.cfg.BatchSize),
	})
	if err != nil {
		return nil, fmt.Errorf("select due hosts: %w", err)
	}
	if len(due) == 0 {
		return nil, ErrNothingDue
	}

	def, err := s.create(ctx, q, org, program, KindVerification, rung, len(due))
	if err != nil {
		return nil, err
	}

	targets := make([]sqlcgen.AddRunTargetsParams, 0, len(due))
	keys := make([]string, 0, len(due))
	for _, row := range due {
		targets = append(targets, sqlcgen.AddRunTargetsParams{
			RunID:   pgUUID(def.RunID),
			AssetID: row.AssetID,
			OrgID:   pgUUID(org),
			Key:     row.Key,
		})
		keys = append(keys, row.Key)
	}
	if _, err := q.AddRunTargets(ctx, targets); err != nil {
		return nil, fmt.Errorf("freeze target list: %w", err)
	}

	def.Env["FASTRECON_TARGETS_URL"] = s.targetsURL(def.RunID, def.Deadline)
	def.Env["FASTRECON_STAGES"] = stagesFor(rung)
	s.log.InfoContext(ctx, "verification run defined",
		"run", def.RunID, "program", program, "rung", rung, "targets", len(keys))
	return def, nil
}

// Discovery provisions one enumeration over a programme's apexes.
//
// A discovery run gets no targets URL and a domain instead. That is the whole
// difference between the two mandates.
func (s *Scheduler) Discovery(
	ctx context.Context, q *sqlcgen.Queries, org, program uuid.UUID,
) (*Definition, error) {
	at := s.now()
	apexes, err := q.ApexesForProgram(ctx, sqlcgen.ApexesForProgramParams{
		ProgramID: pgUUID(program),
		At:        stamp(at),
	})
	if err != nil {
		return nil, fmt.Errorf("read perimeter: %w", err)
	}
	if len(apexes) == 0 {
		return nil, ErrNoPerimeter
	}

	def, err := s.create(ctx, q, org, program, KindDiscovery, "enum", 0)
	if err != nil {
		return nil, err
	}
	def.Env["FASTRECON_DOMAIN"] = strings.Join(apexes, ",")
	def.Env["FASTRECON_STAGES"] = stagesFor("enum")

	// Written at creation rather than at completion. A run that dies on the
	// way must not be restarted by the cadence: the deadline sweeper already
	// handles it, and confusing the two would start two.
	if err := q.TouchDiscovery(ctx, sqlcgen.TouchDiscoveryParams{
		ProgramID: pgUUID(program),
		At:        stamp(at),
	}); err != nil {
		return nil, fmt.Errorf("record discovery: %w", err)
	}

	s.log.InfoContext(ctx, "discovery run defined",
		"run", def.RunID, "program", program, "apexes", len(apexes))
	return def, nil
}

// create writes the row and builds everything a run receives.
func (s *Scheduler) create(
	ctx context.Context, q *sqlcgen.Queries, org, program uuid.UUID,
	kind, scope string, targets int,
) (*Definition, error) {
	live, err := s.InFlight(ctx, q, program, kind)
	if err != nil {
		return nil, err
	}
	if live != nil {
		return nil, fmt.Errorf("%w: %s", ErrRunInFlight, live.Error())
	}

	program1, err := q.ProgramForScheduling(ctx, sqlcgen.ProgramForSchedulingParams{
		ProgramID: pgUUID(program),
	})
	if err != nil {
		return nil, fmt.Errorf("read programme: %w", err)
	}
	if reason, ok := authorized(program1, s.now()); !ok {
		return nil, fmt.Errorf("the programme is not authorized: %s", reason)
	}

	at := s.now()
	def := &Definition{
		RunID:       uuid.New(),
		OrgID:       org,
		ProgramID:   program,
		Kind:        kind,
		Scope:       scope,
		Deadline:    at.Add(s.cfg.Timeout),
		TargetCount: targets,
	}

	params := sqlcgen.CreateRunParams{
		ID:        pgUUID(def.RunID),
		OrgID:     pgUUID(org),
		ProgramID: pgUUID(program),
		Kind:      kind,
		Scope:     scope,
		Deadline:  stamp(def.Deadline),
	}
	if targets > 0 {
		count := bounded(targets)
		params.TargetCount = &count
	}
	if err := q.CreateRun(ctx, params); err != nil {
		// The index is the reservation, and the check above is only the
		// readable half of it. Two transactions that never saw each other's
		// rows both reach here, and one of them loses.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return nil, fmt.Errorf("%w: another run of the same kind was created first", ErrRunInFlight)
		}
		return nil, fmt.Errorf("create run: %w", err)
	}

	// The report token lives as long as the deadline plus a margin: a run that
	// spends its whole budget must still be able to deliver what it produced.
	report := s.signer.Mint(auth.PurposeReport, def.RunID, def.Deadline.Add(s.cfg.Grace))
	def.Env = map[string]string{
		"FASTRECON_PORTS":          s.cfg.Ports,
		"FASTRECON_SCAN_RATE":      strconv.Itoa(int(program1.RateLimitRps)),
		"FASTRECON_WEBHOOK_URL":    strings.TrimSuffix(s.cfg.PublicURL, "/") + "/reports",
		"FASTRECON_WEBHOOK_HEADER": "Authorization: Bearer " + report,
		"FASTRECON_TIMEOUT":        s.cfg.Timeout.String(),
	}
	return def, nil
}

// InFlight reads the run a second request would run into, or nil.
func (s *Scheduler) InFlight(
	ctx context.Context, q *sqlcgen.Queries, program uuid.UUID, kind string,
) (*InFlight, error) {
	row, err := q.LiveRunForProgram(ctx, sqlcgen.LiveRunForProgramParams{
		ProgramID: pgUUID(program),
		Kind:      kind,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read live run: %w", err)
	}

	live := &InFlight{
		ID:        uuid.UUID(row.ID.Bytes),
		Kind:      row.Kind,
		Scope:     row.Scope,
		State:     row.State,
		CreatedAt: row.CreatedAt.Time,
		Deadline:  row.Deadline.Time,
	}
	if row.StartedAt.Valid {
		started := row.StartedAt.Time
		live.StartedAt = &started
	}
	if row.TargetCount != nil {
		live.Targets = int(*row.TargetCount)
	}
	return live, nil
}

// Sweep expires runs whose deadline passed with nothing delivered.
//
// It repairs nothing, and that is the property the whole design rests on. Due
// dates move only when a report is ingested, so an abandoned run leaves the
// inventory exactly as it found it; expiring it frees its targets and makes the
// failure visible.
func (s *Scheduler) Sweep(ctx context.Context, q *sqlcgen.Queries) (int, error) {
	at := s.now()
	expired, err := q.ExpireRuns(ctx, sqlcgen.ExpireRunsParams{At: stamp(at)})
	if err != nil {
		return 0, fmt.Errorf("expire runs: %w", err)
	}
	for _, run := range expired {
		s.log.WarnContext(ctx, "run expired",
			"run", uuid.UUID(run.ID.Bytes), "program", uuid.UUID(run.ProgramID.Bytes),
			"kind", run.Kind, "scope", run.Scope,
			"claimed", run.StartedAt.Valid, "targets", run.TargetCount)
	}
	return len(expired), nil
}

// targetsURL signs a link that is readable for the life of the run.
//
// The signature covers the run, the purpose and the expiry, so a token minted
// to fetch a target list cannot be replayed to post a report, and there is
// nothing to revoke: the run's own state is the revocation.
func (s *Scheduler) targetsURL(run uuid.UUID, deadline time.Time) string {
	token := s.signer.Mint(auth.PurposeTargets, run, deadline)
	return fmt.Sprintf("%s/runs/%s/targets?token=%s",
		strings.TrimSuffix(s.cfg.PublicURL, "/"), run, url.QueryEscape(token))
}

// stagesFor maps a rung onto the ladder the scanner runs.
//
// An asset due for full does not need a resolve run: full runs every rung below
// it. That is what makes a hand entered host cheap to satisfy in one pass, and
// it is the reason the ladder is the cost knob rather than a cadence per probe
// type.
func stagesFor(scope string) string {
	switch scope {
	case lifecycle.RungResolve:
		return "resolve"
	case "enum":
		return "enumerate,exclude,resolve,portscan,httpprobe"
	default:
		return "resolve,portscan,httpprobe"
	}
}

func authorized(p sqlcgen.ProgramForSchedulingRow, now time.Time) (string, bool) {
	if p.State != "active" {
		return "it is " + p.State, false
	}
	if p.AuthorizedFrom.Valid && now.Before(p.AuthorizedFrom.Time) {
		return "the authorization has not started", false
	}
	if p.AuthorizedTo.Valid && !now.Before(p.AuthorizedTo.Time) {
		return "the authorization has expired", false
	}
	return "", true
}

// bounded narrows a count to the column that holds it. Both callers are
// already bounded by configuration; the clamp is here so that a setting nobody
// validated cannot turn a batch size into a negative limit.
func bounded(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < 0 {
		return 0
	}
	return int32(n)
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// Sweeper expires runs whose deadline passed.
//
// It is a loop of its own rather than a branch of the housekeeping tick,
// because the two answer to different clocks: partitions are created months
// ahead and a run holds its targets for minutes. A run nothing expires holds
// them forever, and the assets it froze are invisible to every later tick.
type Sweeper struct {
	scheduler *Scheduler
	queries   *sqlcgen.Queries
	interval  time.Duration
	log       *slog.Logger
}

// NewSweeper builds the loop. It takes no enable flag, for the same reason the
// partition job does not.
func NewSweeper(scheduler *Scheduler, queries *sqlcgen.Queries, interval time.Duration, log *slog.Logger) *Sweeper {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Sweeper{scheduler: scheduler, queries: queries, interval: interval, log: log}
}

// Run ticks until the context ends.
func (s *Sweeper) Run(ctx context.Context) {
	s.Once(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Once(ctx)
		}
	}
}

// Once does a single pass, which is also what a test calls.
func (s *Sweeper) Once(ctx context.Context) {
	if _, err := s.scheduler.Sweep(ctx, s.queries); err != nil {
		s.log.ErrorContext(ctx, "run sweep failed", "error", err)
	}
}
