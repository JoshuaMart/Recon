// Package runs decides what to scan next, freezes it, and hands out the
// definition an execution needs.
//
// There is no queue table and no lease column: a run's frozen target list is
// its reservation, selection skips what a live run holds, and the reservation
// expires with the run. Due dates only move when a report is ingested, so a
// dead run takes nothing with it.
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

// Kinds of run. A discovery run is given a domain and may find things; a
// verification run is given a list, the only case where a missing answer means
// something.
const (
	KindDiscovery    = "discovery"
	KindVerification = "verification"
	// KindCandidate is the lane for Certificate Transparency candidates.
	//
	// A third kind rather than a second reservation scheme (the bound is already
	// per kind). It exists because a verification run holds its slot for its whole
	// deadline, while a candidate's first check is due at sixty seconds.
	KindCandidate = "candidate"
)

// Errors a caller acts on.
var (
	// ErrNothingDue is not a failure: it is the normal state of a healthy inventory.
	ErrNothingDue = errors.New("nothing is due")
	// ErrRunInFlight carries the run itself, for someone to decide about.
	ErrRunInFlight = errors.New("a run is already in flight")
	// ErrNoPerimeter is a programme with no apex to enumerate.
	ErrNoPerimeter = errors.New("the programme declares no apex")
	// ErrRunNotFound is a run this organization does not have.
	ErrRunNotFound = errors.New("no such run")
	// ErrRunClosed is a run that already finished.
	ErrRunClosed = errors.New("the run is already closed")
	// ErrReportInProgress is a run whose report is being written. Closing it now
	// would refuse work that was done.
	ErrReportInProgress = errors.New("the run's report is being written")
)

// InFlight is what a refusal says. A run a scanner opened is one to wait for;
// one nobody claimed is a failed provisioning. StartedAt, written by the first
// report, tells them apart.
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

// Age is how long the run has existed.
func (f InFlight) Age(now time.Time) time.Duration { return now.Sub(f.CreatedAt) }

// Claimed reports whether a scanner ever opened the run.
func (f InFlight) Claimed() bool { return f.StartedAt != nil }

func (f InFlight) Error() string {
	claimed := "nothing has opened it"
	if f.Claimed() {
		claimed = "a scanner opened it at " + f.StartedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("run %s (%s, %s) is %s and %s", f.ID, f.Kind, f.Scope, f.State, claimed)
}

// Definition is everything an execution receives. It opens nothing in the
// inventory: both credentials are HMACs over run, purpose and expiry, so there
// is nothing to store, revoke or purge.
type Definition struct {
	RunID       uuid.UUID
	OrgID       uuid.UUID
	ProgramID   uuid.UUID
	Kind        string
	Scope       string
	Deadline    time.Time
	TargetCount int
	// Apex is the single root domain a discovery run enumerates; empty on a
	// verification.
	//
	// One domain, not the perimeter, because the scanner's -d takes one value. A
	// programme with several apexes gets one run each, which also keeps a failed
	// enumeration attributable.
	Apex string
	// Args is the whole invocation, kept apart from Env.
	//
	// A start call replaces the definition's arguments but merges its environment
	// (measured). A flag on the definition would therefore beat a variable sent
	// here and silently pin every run to one domain.
	Args []string
	// Env is what the definition does not already carry. It merges, so the source
	// API keys the definition holds survive; the control plane names secrets, never
	// carries them.
	Env map[string]string
	// ExternalID is the platform's name for the execution, set once started. It is
	// the only handle on the logs of a failed run.
	ExternalID string
}

// Platform starts a run definition.
//
// The control plane starts, it never updates: the call that modifies a
// definition replaces its whole environment, which would wipe the source API
// keys without any error.
type Platform interface {
	// Start returns the platform's identifier for the execution.
	Start(ctx context.Context, def *Definition) (string, error)
	// Status reads what became of an execution.
	Status(ctx context.Context, externalID string) (Execution, error)
	// Stop asks the platform to end an execution; one that already ended is not an
	// error.
	Stop(ctx context.Context, externalID string) error
	// Name is what a log line calls this platform.
	Name() string
}

// Execution is the platform's record of a run, read back. It holds only the
// facts the sweeper acts on; an unknown state is neither running nor failed and
// leaves the run to its deadline.
type Execution struct {
	// Running is true once a container is executing the run.
	Running bool
	// Failed is true once the execution ended without success (non-zero exit,
	// cancelled or interrupted).
	Failed bool
	// Reason is the platform's word for the ending, never its log tail (that is the
	// scanner's report).
	Reason string
	// ExitCode is the scanner's own status, when the platform reports one.
	ExitCode *int
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

// WithClock replaces the instant deadlines are measured from, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithPlatform sets what starts a definition. Without one, runs are defined but
// never started.
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

// Signer and Config let a test build a second scheduler over the same
// deployment with another platform.
func (s *Scheduler) Signer() *auth.Signer        { return s.signer }
func (s *Scheduler) Config() config.Verification { return s.cfg }

// Cadence is shared with the ingestor so both read one setting.
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
// It shares a transaction with the selection: without it, two ticks reading the
// same due rows would hand the same hosts to two runs.
func (s *Scheduler) Verification(
	ctx context.Context, q *sqlcgen.Queries, org, program uuid.UUID, rung string,
) (*Definition, error) {
	if rung != lifecycle.RungResolve && rung != lifecycle.RungFull {
		return nil, fmt.Errorf("unknown rung %q", rung)
	}

	// Checked first: a programme with a run in flight has nothing due (selection
	// skips the frozen list), and "nothing is due" would hide that run.
	if live, err := s.InFlight(ctx, q, program, KindVerification); err != nil {
		return nil, err
	} else if live != nil {
		return nil, fmt.Errorf("%w: %s", ErrRunInFlight, live.Error())
	}

	// Also not while an enumeration walks the same perimeter: a discovery run freezes nothing, so
	// selection cannot see its hosts, and its report has not landed, so their due dates are
	// unchanged. One direction only: discovery is rare and its cadence a promise, while
	// verification re-selects next tick; the symmetric rule would starve discovery, a
	// verification being in flight most minutes during a drain.
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

	def, err := s.create(ctx, q, org, program, KindVerification, rung, "", len(due), s.cfg.Timeout)
	if err != nil {
		return nil, err
	}

	// One loop keeps both arrays the same length; the statement indexes them
	// together and a short one reads as nulls instead of failing.
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

	// The list travels in a header, not the URL: a credential in a query string
	// ends up in every access, proxy and error log.
	def.Args = append(def.Args,
		"--stages", rung,
		"--targets-url", s.targetsURL(def.RunID),
		"--targets-header", "Authorization: Bearer "+s.mint(auth.PurposeTargets, def.RunID, def.Deadline),
	)
	s.log.InfoContext(ctx, "verification run defined",
		"run", def.RunID, "program", program, "rung", rung, "targets", len(keys))
	return def, nil
}

// Candidate freezes the candidates that are due and returns their run. Same shape as
// Verification but not a parameter of it, as it deliberately skips a check: a live
// verification (the reason the lane exists) or discovery does not hold it back, since a
// resolve sends nothing to the target. Another candidate run does (partial unique index), as
// does the frozen list: a host held by a verification run is not taken, the lease being
// shared across lanes.
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

	// Shorter deadline than a sweep: one rung over a short list must not hold the
	// slot for thirty minutes.
	def, err := s.create(ctx, q, org, program, KindCandidate,
		lifecycle.RungResolve, "", len(due), s.cfg.CandidateTimeout)
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

	// Stage 1 is replaced by the targets input, so no enumeration runs and no
	// source quota is spent whatever the list size.
	def.Args = append(def.Args,
		"--stages", lifecycle.RungResolve,
		"--targets-url", s.targetsURL(def.RunID),
		"--targets-header", "Authorization: Bearer "+s.mint(auth.PurposeTargets, def.RunID, def.Deadline),
	)
	s.log.InfoContext(ctx, "candidate run defined",
		"run", def.RunID, "program", program, "targets", len(keys))
	return def, nil
}

// Discovery provisions one enumeration per apex of a programme.
//
// The scanner takes one root domain per execution, so three apexes are three
// runs, never one run naming three. Apexes already in flight are skipped so the
// free ones still go out; only a programme with none free answers
// ErrRunInFlight.
func (s *Scheduler) Discovery(
	ctx context.Context, q *sqlcgen.Queries, org, program uuid.UUID,
) ([]*Definition, error) {
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

	// Built once and appended to each definition. The perimeter is the
	// programme's, not the apex's: a scanner told less than the whole list sends
	// packets where the perimeter says not to.
	//
	// Exclusions are name patterns, so address-range or path rules cannot travel.
	// They still classify at ingestion, after the packet. That gap is logged, not
	// dropped.
	var exclusions, untravelled []string
	for _, rule := range excluded {
		switch rule.Matcher {
		case "apex", "fqdn":
			exclusions = append(exclusions, "--exclude", rule.Pattern)
		default:
			untravelled = append(untravelled, rule.Matcher+":"+rule.Pattern)
		}
	}
	if len(untravelled) > 0 {
		s.log.WarnContext(ctx, "exclusions the scanner cannot be given",
			"program", program, "rules", untravelled,
			"effect", "those hosts are probed and classified out of scope afterwards")
	}

	defs := make([]*Definition, 0, len(apexes))
	var inFlight error
	for _, apex := range apexes {
		def, err := s.create(ctx, q, org, program, KindDiscovery, ScopeFull, apex, 0, s.cfg.Timeout)
		switch {
		case errors.Is(err, ErrRunInFlight):
			// Kept so the free apexes still go out; only the answer if none was.
			inFlight = err
			continue
		case err != nil:
			return nil, err
		}
		// -d is a single value on the scanner side: a comma-joined list reaches it as
		// one malformed domain and the run dies before resolving anything.
		def.Args = append(def.Args, "--stages", def.Scope, "-d", apex)
		def.Args = append(def.Args, exclusions...)
		defs = append(defs, def)
	}
	if len(defs) == 0 {
		if inFlight != nil {
			return nil, inFlight
		}
		return nil, ErrNoPerimeter
	}

	// Written at creation, not completion. A run that dies must not be restarted by
	// the cadence; the deadline sweeper handles it.
	if err := q.TouchDiscovery(ctx, sqlcgen.TouchDiscoveryParams{
		ProgramID: pgUUID(program),
		At:        stamp(at),
	}); err != nil {
		return nil, fmt.Errorf("record discovery: %w", err)
	}

	s.log.InfoContext(ctx, "discovery runs defined",
		"program", program, "runs", len(defs), "apexes", len(apexes))
	return defs, nil
}

// create writes the run row and builds everything the run receives.
func (s *Scheduler) create(
	ctx context.Context, q *sqlcgen.Queries, org, program uuid.UUID,
	kind, scope, apex string, targets int, timeout time.Duration,
) (*Definition, error) {
	// The bound is per apex for a discovery and per programme for a verification,
	// the unit each one holds.
	live, err := s.inFlightFor(ctx, q, program, kind, apex)
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
		Apex:        apex,
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
	// Null rather than "": the column's constraint ties it to the kind and would
	// accept "" while meaning nothing.
	if apex != "" {
		params.Apex = &apex
	}
	if targets > 0 {
		count := bounded(targets)
		params.TargetCount = &count
	}
	if err := q.CreateRun(ctx, params); err != nil {
		// The unique index is the real reservation; the check above is the readable
		// half. Two transactions that never saw each other's rows both reach here.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return nil, fmt.Errorf("%w: another run of the same kind was created first", ErrRunInFlight)
		}
		return nil, fmt.Errorf("create run: %w", err)
	}

	// Valid for the deadline plus a margin, so a run that spends its whole budget
	// can still deliver.
	report := s.signer.Mint(auth.PurposeReport, def.RunID, def.Deadline.Add(s.cfg.Grace))
	def.Args = []string{
		"--ports", s.cfg.Ports,
		"--scan-rate", strconv.Itoa(int(program1.RateLimitRps)),
		"--webhook-url", strings.TrimSuffix(s.cfg.PublicURL, "/") + "/reports",
		"--webhook-header", "Authorization: Bearer " + report,
		// The scanner's thirty second default is shorter than writing a large report:
		// it hung up, the write rolled back, and retries repeated the same work.
		"--webhook-timeout", s.cfg.ReportTimeout.String(),
		"--timeout", timeout.String(),
	}
	def.Env = map[string]string{}
	return def, nil
}

// mint signs the targets credential.
func (s *Scheduler) mint(purpose auth.Purpose, run uuid.UUID, expires time.Time) string {
	return s.signer.Mint(purpose, run, expires)
}

// InFlight reads the run a second request would collide with, or nil.
func (s *Scheduler) InFlight(
	ctx context.Context, q *sqlcgen.Queries, program uuid.UUID, kind string,
) (*InFlight, error) {
	return s.inFlightFor(ctx, q, program, kind, "")
}

// liveRun is the common shape of both live run statements; the conversion makes
// the compiler check that they stay identical.
type liveRun sqlcgen.LiveRunForProgramRow

// inFlightFor answers on the unit the kind holds: the programme for a
// verification (its frozen list can name any host), one apex for a discovery
// (so one apex in flight does not hide the free ones).
//
// An empty apex keeps the programme-wide question, used to block a verification
// behind any live enumeration.
func (s *Scheduler) inFlightFor(
	ctx context.Context, q *sqlcgen.Queries, program uuid.UUID, kind, apex string,
) (*InFlight, error) {
	var (
		row liveRun
		err error
	)
	if kind == KindDiscovery && apex != "" {
		var got sqlcgen.LiveDiscoveryForApexRow
		got, err = q.LiveDiscoveryForApex(ctx, sqlcgen.LiveDiscoveryForApexParams{
			ProgramID: pgUUID(program),
			Apex:      apex,
		})
		row = liveRun(got)
	} else {
		var got sqlcgen.LiveRunForProgramRow
		got, err = q.LiveRunForProgram(ctx, sqlcgen.LiveRunForProgramParams{
			ProgramID: pgUUID(program),
			Kind:      kind,
		})
		row = liveRun(got)
	}
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
// It repairs nothing: due dates only move on ingestion, so an abandoned run
// leaves the inventory as it was. Expiring it frees its targets and makes the
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

// targetsURL is where a run fetches its frozen list. It carries no credential,
// which travels in a header (see Verification).
func (s *Scheduler) targetsURL(run uuid.UUID) string {
	return fmt.Sprintf("%s/runs/%s/targets", strings.TrimSuffix(s.cfg.PublicURL, "/"), run)
}

// credentialFlags are the arguments whose value is a live bearer token (report
// posting, target fetching). Anything printing an invocation goes through
// Redacted.
var credentialFlags = map[string]struct{}{
	"--webhook-header": {},
	"--targets-header": {},
}

// Redacted returns an invocation safe to log.
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

// Scopes a run may be given, using the scanner's own names. Each rung runs the
// ones below it, so the ladder is the cost knob. They match the values the run
// row is constrained to.
const (
	// ScopeFull walks every rung and is what a discovery run does. The scanner
	// also accepts enum, resolve and ports; only provisioned ones are named here.
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

// bounded narrows a count to its column and clamps unvalidated settings, so
// none can become a negative limit.
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

// Recorder writes the name the platform gave an execution.
//
// A function rather than a database handle: the write happens after a cloud API
// call and must not sit in a transaction held open during it, and the caller
// alone knows the organization.
type Recorder func(ctx context.Context, runID uuid.UUID, externalID string) error

// Launch starts a definition and records what the platform called it.
//
// It runs after the creating transaction commits. Starting inside it could
// leave a live execution against a rolled-back row. Starting after means a
// refusing platform leaves the row pending: the sweeper expires it and the next
// tick starts a fresh run, since due dates never moved.
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
		// The execution is running; only its name is lost, along with that run's logs.
		// Worth an error, not a rollback.
		return fmt.Errorf("record the start of %s: %w", def.RunID, err)
	}

	s.log.InfoContext(ctx, "run started",
		"run", def.RunID, "kind", def.Kind, "scope", def.Scope,
		"platform", s.platform.Name(), "external", external)
	return nil
}

// exitMeanings are the scanner's exit statuses from its output contract.
var exitMeanings = map[int]string{
	1: "the scanner refused its configuration",
	2: "the report was delivered with the scope unfinished",
	3: "the report was produced and could not be delivered",
	4: "the scanner produced no report",
}

// platformFailure is the error text for a run the platform ended.
func platformFailure(execution Execution) string {
	message := "the platform ended the execution"
	if execution.Reason != "" {
		message += " (" + execution.Reason + ")"
	}
	if execution.ExitCode != nil {
		message += fmt.Sprintf(" with exit code %d", *execution.ExitCode)
		if meaning, ok := exitMeanings[*execution.ExitCode]; ok {
			message += ": " + meaning
		}
	}
	return message
}

// Watch asks the platform what became of every open run it started and returns
// how many it closed.
//
// A report closes a run and a dead run delivers none, so the deadline used to
// be the only way out. A platform that cannot be read is logged and skipped
// per run, leaving it to its deadline.
func (s *Scheduler) Watch(ctx context.Context, q *sqlcgen.Queries) (int, error) {
	if s.platform == nil {
		return 0, nil
	}
	open, err := q.OpenRunsOnPlatform(ctx)
	if err != nil {
		return 0, fmt.Errorf("read open runs: %w", err)
	}

	closed := 0
	for _, run := range open {
		if run.ExternalID == nil {
			continue
		}
		id := uuid.UUID(run.ID.Bytes)
		execution, err := s.platform.Status(ctx, *run.ExternalID)
		if err != nil {
			s.log.WarnContext(ctx, "the platform could not say what became of a run",
				"run", id, "external", *run.ExternalID, "error", err)
			continue
		}

		switch {
		case execution.Failed:
			reason := platformFailure(execution)
			_, err := q.FailRun(ctx, sqlcgen.FailRunParams{
				RunID: run.ID, At: stamp(s.now()), Error: reason,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				// Its report is being written, or it closed since the read.
				continue
			}
			if err != nil {
				return closed, fmt.Errorf("close run %s: %w", id, err)
			}
			closed++
			s.log.WarnContext(ctx, "run failed on the platform",
				"run", id, "program", uuid.UUID(run.ProgramID.Bytes),
				"kind", run.Kind, "scope", run.Scope, "external", *run.ExternalID, "reason", reason)
		case execution.Running && !run.StartedAt.Valid:
			if err := q.StartRunOnPlatform(ctx, sqlcgen.StartRunOnPlatformParams{
				RunID: run.ID, At: stamp(s.now()),
			}); err != nil {
				return closed, fmt.Errorf("mark run %s running: %w", id, err)
			}
		}
	}
	return closed, nil
}

// cancelled is the error of a run closed from the console.
const cancelled = "cancelled from the console"

// Cancel closes an open run and returns the platform's name for its execution
// (empty if none). The caller commits, then calls Stop: asking the platform
// inside the transaction would hold a connection for as long as it takes to
// answer.
func (s *Scheduler) Cancel(ctx context.Context, q *sqlcgen.Queries, runID uuid.UUID) (string, error) {
	row, err := q.FailRun(ctx, sqlcgen.FailRunParams{
		RunID: pgUUID(runID), At: stamp(s.now()), Error: cancelled,
	})
	if err == nil {
		if row.ExternalID == nil {
			return "", nil
		}
		return *row.ExternalID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("close run: %w", err)
	}

	// Nothing was closed; the three reasons need different answers.
	run, err := q.RunForTargets(ctx, sqlcgen.RunForTargetsParams{RunID: pgUUID(runID)})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrRunNotFound
	case err != nil:
		return "", fmt.Errorf("read run: %w", err)
	case run.State == "pending" || run.State == "running":
		return "", ErrReportInProgress
	default:
		return "", fmt.Errorf("%w: it is %s", ErrRunClosed, run.State)
	}
}

// Stop asks the platform to end an execution; with no platform there is nothing
// to stop.
func (s *Scheduler) Stop(ctx context.Context, externalID string) (bool, error) {
	if s.platform == nil || externalID == "" {
		return false, nil
	}
	if err := s.platform.Stop(ctx, externalID); err != nil {
		return false, err
	}
	return true, nil
}

// Sweeper expires runs past their deadline. It is its own loop because the
// clocks differ: partitions are made months ahead while a run holds its targets
// for minutes, and an unexpired run hides its assets from every later tick.
type Sweeper struct {
	scheduler *Scheduler
	queries   *sqlcgen.Queries
	interval  time.Duration
	log       *slog.Logger
}

// NewSweeper builds the loop. It takes no enable flag, like the partition job.
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

// Once does a single pass (also what tests call). The deadline goes first: a
// run past it expires whatever the platform says.
func (s *Sweeper) Once(ctx context.Context) {
	if _, err := s.scheduler.Sweep(ctx, s.queries); err != nil {
		s.log.ErrorContext(ctx, "run sweep failed", "error", err)
	}
	if _, err := s.scheduler.Watch(ctx, s.queries); err != nil {
		s.log.ErrorContext(ctx, "platform watch failed", "error", err)
	}
}
