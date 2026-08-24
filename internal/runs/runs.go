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
	// KindCandidate is the lane a Certificate Transparency candidate takes.
	//
	// A third value rather than a second reservation scheme: the bound is
	// already per kind, so this costs a partial unique index and a selection.
	// It exists because one live verification run holds its slot for its whole
	// deadline, and the aggressive curve rests on a candidate's first check
	// happening at sixty seconds.
	KindCandidate = "candidate"
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
	// Args is the whole invocation, and it is deliberately not split with Env.
	//
	// A start call replaces the definition's arguments wholesale and merges
	// into its environment, which was measured rather than assumed. So a flag
	// on the definition beats a variable sent here: a definition carrying
	// "-d hackerone.com" would make every run scan that whatever the
	// environment said, and nothing would look wrong. Sending the invocation as
	// arguments removes that class of surprise entirely, and it makes the run
	// record say what the run was actually asked to do.
	Args []string
	// Env is what the definition does not already carry. It merges, so the
	// source API keys the definition holds survive: the control plane names
	// secrets and never carries them.
	Env map[string]string
	// ExternalID is what the platform called this execution, filled in once it
	// has been started. It is the only handle on the logs of a run that went
	// wrong.
	ExternalID string
}

// Platform starts a run definition.
//
// The control plane starts, it never updates. The call that modifies a
// definition replaces its whole environment map, so a control plane that wrote
// there would wipe the source API keys the definition carries, and nothing
// would fail: the next run would simply query fewer sources and find less.
type Platform interface {
	// Start returns the platform's own identifier for the execution.
	Start(ctx context.Context, def *Definition) (string, error)
	// Name is what a log line calls this platform.
	Name() string
}

// Scheduler provisions runs.
type Scheduler struct {
	signer   *auth.Signer
	cfg      config.Verification
	platform Platform
	now      func() time.Time
	log      *slog.Logger
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

// WithPlatform is what actually starts a definition. Without one the scheduler
// defines runs and starts nothing, which is a deployment that only probes what
// somebody launches by hand.
func WithPlatform(platform Platform) Option {
	return func(s *Scheduler) { s.platform = platform }
}

// New builds a scheduler.
func New(signer *auth.Signer, cfg config.Verification, log *slog.Logger, opts ...Option) *Scheduler {
	scheduler := &Scheduler{signer: signer, cfg: cfg, now: time.Now, log: log}
	for _, opt := range opts {
		opt(scheduler)
	}
	return scheduler
}

// Signer and Config are what a second scheduler over the same deployment is
// built from, which is what a test that swaps the platform needs.
func (s *Scheduler) Signer() *auth.Signer        { return s.signer }
func (s *Scheduler) Config() config.Verification { return s.cfg }

// Cadence is what the ingestor reschedules with, so both read one setting.
func (s *Scheduler) Cadence() lifecycle.Cadence {
	return lifecycle.Cadence{
		Resolve:        s.cfg.Resolve,
		Full:           s.cfg.Full,
		Fingerprint:    s.cfg.Fingerprint,
		Inactive:       s.cfg.Inactive,
		Jitter:         s.cfg.Jitter,
		FullFloor:      s.cfg.FullFloor,
		RenderSole:     s.cfg.RenderSole,
		RenderRecovery: s.cfg.RenderRecovery,
		RenderBlind:    s.cfg.RenderBlind,
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

	// And not while an enumeration is walking the same perimeter, which the
	// frozen list cannot express: a discovery run freezes nothing, because it is
	// the one allowed to find things, so selection has no way to see the hosts
	// it is already scanning.
	//
	// The window is the run itself. Its report has not landed, so every asset it
	// is touching still carries the due date it had before, and a verification
	// starting a minute in selects hosts a browser-less scanner is connecting to
	// right now. Two runs holding the same host is double scan traffic against
	// somebody's perimeter, which is the one cost this system is not allowed to
	// be careless with, and the existing lease only ever covered the pair it
	// could see.
	//
	// One direction only. Discovery is rare and its cadence is a promise, so it
	// wins; verification re-selects on the next tick and loses nothing but
	// minutes. Blocking discovery on a live verification would be the symmetric
	// rule and a starvation: during a drain a verification is in flight most
	// minutes, and the enumeration would never go out.
	if live, err := s.InFlight(ctx, q, program, KindDiscovery); err != nil {
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

	def, err := s.create(ctx, q, org, program, KindVerification, rung, len(due), s.cfg.Timeout)
	if err != nil {
		return nil, err
	}

	// Built in one loop, which is what keeps the two arrays the same length:
	// the statement indexes them together and a short one would read as nulls
	// rather than fail.
	assets := make([]pgtype.UUID, 0, len(due))
	keys := make([]string, 0, len(due))
	for _, row := range due {
		assets = append(assets, row.AssetID)
		keys = append(keys, row.Key)
	}
	if _, err := q.AddRunTargets(ctx, sqlcgen.AddRunTargetsParams{
		RunID:    pgUUID(def.RunID),
		OrgID:    pgUUID(org),
		AssetIds: assets,
		Keys:     keys,
	}); err != nil {
		return nil, fmt.Errorf("freeze target list: %w", err)
	}

	// The list travels behind a header rather than in the URL, which is what
	// the flag exists for: a credential in a query string ends up in every
	// access log, proxy log and error message that ever prints the URL.
	def.Args = append(def.Args,
		"--stages", rung,
		"--targets-url", s.targetsURL(def.RunID),
		"--targets-header", "Authorization: Bearer "+s.mint(auth.PurposeTargets, def.RunID, def.Deadline),
	)
	s.log.InfoContext(ctx, "verification run defined",
		"run", def.RunID, "program", program, "rung", rung, "targets", len(keys))
	return def, nil
}

// Candidate freezes the candidates that are due and returns their run.
//
// The same shape as Verification and deliberately not a parameter of it, because
// what differs is what it does *not* check. A live verification does not hold
// this back, which is the whole reason the lane exists: a sweep holds its slot
// for its whole deadline, and the aggressive curve rests on a candidate's first
// check happening at sixty seconds rather than half an hour later.
//
// Nor does a live discovery, and that is a decision. Discovery blocks
// verification because a full sweep sends a second scanner at hosts the first
// one is connected to. A resolve sends nothing to the target at all, so there is
// nothing to collide with, and blocking here would spend the freshness advantage
// on a run that cannot interfere with it.
//
// What still holds it back is another candidate run, kept by the partial unique
// index, and the frozen list: a host held by a verification run is not taken
// here either, because the lease is one lease across the lanes rather than one
// per lane.
func (s *Scheduler) Candidate(
	ctx context.Context, q *sqlcgen.Queries, org, program uuid.UUID,
) (*Definition, error) {
	if live, err := s.InFlight(ctx, q, program, KindCandidate); err != nil {
		return nil, err
	} else if live != nil {
		return nil, fmt.Errorf("%w: %s", ErrRunInFlight, live.Error())
	}

	at := s.now()
	due, err := q.SelectDueCandidates(ctx, sqlcgen.SelectDueCandidatesParams{
		OrgID:     pgUUID(org),
		ProgramID: pgUUID(program),
		At:        stamp(at),
		Batch:     bounded(s.cfg.BatchSize),
	})
	if err != nil {
		return nil, fmt.Errorf("select due candidates: %w", err)
	}
	if len(due) == 0 {
		return nil, ErrNothingDue
	}

	// A shorter deadline than a sweep, because this does one rung over a short
	// list. A slot held for thirty minutes by a run that had one thing to do
	// turns the bound this lane exists for back into the problem it solves.
	def, err := s.create(ctx, q, org, program, KindCandidate,
		lifecycle.RungResolve, len(due), s.cfg.CandidateTimeout)
	if err != nil {
		return nil, err
	}

	assets := make([]pgtype.UUID, 0, len(due))
	keys := make([]string, 0, len(due))
	for _, row := range due {
		assets = append(assets, row.AssetID)
		keys = append(keys, row.Key)
	}
	if _, err := q.AddRunTargets(ctx, sqlcgen.AddRunTargetsParams{
		RunID:    pgUUID(def.RunID),
		OrgID:    pgUUID(org),
		AssetIds: assets,
		Keys:     keys,
	}); err != nil {
		return nil, fmt.Errorf("freeze target list: %w", err)
	}

	// The targets input is what makes this cheap, and it is the input rather
	// than the length of the list: stage 1 is replaced, so no enumeration runs
	// and no source quota is spent whatever the list holds. One host is the
	// common case, not the mechanism.
	def.Args = append(def.Args,
		"--stages", lifecycle.RungResolve,
		"--targets-url", s.targetsURL(def.RunID),
		"--targets-header", "Authorization: Bearer "+s.mint(auth.PurposeTargets, def.RunID, def.Deadline),
	)
	s.log.InfoContext(ctx, "candidate run defined",
		"run", def.RunID, "program", program, "targets", len(keys))
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

	excluded, err := q.ExclusionsForProgram(ctx, sqlcgen.ExclusionsForProgramParams{
		ProgramID: pgUUID(program),
		At:        stamp(at),
	})
	if err != nil {
		return nil, fmt.Errorf("read exclusions: %w", err)
	}

	def, err := s.create(ctx, q, org, program, KindDiscovery, ScopeFull, 0, s.cfg.Timeout)
	if err != nil {
		return nil, err
	}
	def.Args = append(def.Args, "--stages", def.Scope, "-d", strings.Join(apexes, ","))
	// The exclusion patterns travel with the perimeter. A rule may have changed
	// between a run being defined and a run starting, and they are the second
	// safety net in front of the network rather than a duplicate of the scope.
	//
	// The scanner's exclusions are name patterns, so a rule matching on an
	// address range or a path cannot travel. Those still classify at ingestion,
	// which is after the packet, and that is a real gap in the safety net: it
	// is logged rather than dropped in silence, because a perimeter whose
	// exclusions half apply is worse than one whose limits are known.
	var untravelled []string
	for _, rule := range excluded {
		switch rule.Matcher {
		case "apex", "fqdn":
			def.Args = append(def.Args, "--exclude", rule.Pattern)
		default:
			untravelled = append(untravelled, rule.Matcher+":"+rule.Pattern)
		}
	}
	if len(untravelled) > 0 {
		s.log.WarnContext(ctx, "exclusions the scanner cannot be given",
			"program", program, "run", def.RunID, "rules", untravelled,
			"effect", "those hosts are probed and classified out of scope afterwards")
	}

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
	kind, scope string, targets int, timeout time.Duration,
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
		Deadline:    at.Add(timeout),
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
	def.Args = []string{
		"--ports", s.cfg.Ports,
		"--scan-rate", strconv.Itoa(int(program1.RateLimitRps)),
		"--webhook-url", strings.TrimSuffix(s.cfg.PublicURL, "/") + "/reports",
		"--webhook-header", "Authorization: Bearer " + report,
		"--timeout", timeout.String(),
	}
	def.Env = map[string]string{}
	return def, nil
}

// mint is the targets credential, kept beside the URL that needs it.
func (s *Scheduler) mint(purpose auth.Purpose, run uuid.UUID, expires time.Time) string {
	return s.signer.Mint(purpose, run, expires)
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

// targetsURL is where a run fetches its frozen list.
//
// It carries no credential. The signature travels in a header, because a token
// in a query string is a token in every access log, proxy log and error message
// that ever prints the URL, and those outlive the run by a long way.
func (s *Scheduler) targetsURL(run uuid.UUID) string {
	return fmt.Sprintf("%s/runs/%s/targets", strings.TrimSuffix(s.cfg.PublicURL, "/"), run)
}

// credentialFlags are the arguments whose value is a live credential.
//
// Both are bearer tokens for the life of the run they name: one posts its
// report, the other fetches its frozen target list. Anything that prints an
// invocation has to go through Redacted, which is why the list lives here
// rather than at each call site that might forget it.
var credentialFlags = map[string]struct{}{
	"--webhook-header": {},
	"--targets-header": {},
}

// Redacted is an invocation safe to write down.
func Redacted(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := range out {
		if _, secret := credentialFlags[out[i]]; secret && i+1 < len(out) {
			out[i+1] = "Authorization: Bearer <redacted>"
		}
	}
	return out
}

// Scopes a run may be given, and they are the scanner's own names.
//
// The ladder is a scope rather than a list of stages, and each rung runs the
// ones below it. That is what makes a hand entered host cheap to satisfy in one
// pass, and it is why the ladder is the cost knob rather than a cadence per
// probe type. These are the same four values the run row is constrained to, so
// the column and the flag cannot drift apart.
const (
	// ScopeFull walks every rung, and it is what a discovery run does. The
	// scanner also accepts enum, resolve and ports; only the ones this
	// provisions are named here, so an unused constant cannot drift.
	ScopeFull = "full"
)

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

// interval carries a duration into a statement that compares against one.
func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// Launch starts a definition and records what the platform called it.
//
// It runs **after** the transaction that created the run has committed, and
// that order is the whole of the failure story. Starting inside the transaction
// would leave an execution running with a valid credential against a run row
// that was rolled back. Starting after means a platform that refuses, on a
// quota or an outage, leaves the row pending: the deadline sweeper expires it,
// the signed tokens and the frozen list expire with it, and the due dates were
// never moved, so the next tick starts a fresh run over the same assets.
// Nothing has to be repaired.
// Recorder writes the name the platform gave an execution.
//
// A function rather than a handle on the database, because the write happens
// *after* a call to a cloud API and must not happen inside a transaction that
// was open during it. A pool of ten connections cannot absorb one held for as
// long as somebody else's control plane takes to answer, and the caller is
// also the only one that knows which organization the transaction belongs to.
type Recorder func(ctx context.Context, runID uuid.UUID, externalID string) error

func (s *Scheduler) Launch(ctx context.Context, record Recorder, def *Definition) error {
	if s.platform == nil {
		return nil
	}

	external, err := s.platform.Start(ctx, def)
	if err != nil {
		return fmt.Errorf("start %s run %s: %w", def.Kind, def.RunID, err)
	}
	def.ExternalID = external
	if external == "" {
		return nil
	}

	if err := record(ctx, def.RunID, external); err != nil {
		// The execution is running and this is only its name. Losing it costs
		// the logs of that run, which is worth an error and not a rollback.
		return fmt.Errorf("record the start of %s: %w", def.RunID, err)
	}

	s.log.InfoContext(ctx, "run started",
		"run", def.RunID, "kind", def.Kind, "scope", def.Scope,
		"platform", s.platform.Name(), "external", external)
	return nil
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
