package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/enrich"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// deadlineMargin is how long past its own timeout a run may stay running
// before the sweeper calls it timed out.
const deadlineMargin = 10 * time.Minute

// Scheduler starts queued runs when a slot is free and ingests their reports.
type Scheduler struct {
	Store   *store.Store
	Runner  Runner // nil leaves runs queued
	Log     *slog.Logger
	Slots   int
	Timeout time.Duration
	Ports   []int
	Tick    time.Duration
	Now     func() time.Time

	// Verification, when on, queues runs over due hosts, Batch at a time.
	Verification bool
	Batch        int
	ArchiveAfter time.Duration
	Cadences     *lifecycle.Cadences
	Signatures   []ingest.Signature
	Rand         func() float64
	GeoIP        *enrich.GeoIP

	// PublicURL is where a recon message's link points.
	PublicURL string
	// OnIngested is called after a report is ingested: the saved searches
	// have something new to evaluate. Nil does nothing.
	OnIngested func()

	wg sync.WaitGroup
}

func (s *Scheduler) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

// Recover fails the runs a stopped process left running: a synchronous call
// does not survive its caller. A recon among them is announced, as any recon
// that ends.
func (s *Scheduler) Recover(ctx context.Context) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Store.Q.WithTx(tx)
	now := s.now()
	rows, err := q.FailInterruptedRuns(ctx, now)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Kind == "recon" {
			if err := s.reconEnded(ctx, q, r.ID, r.WildcardID, "failed: interrupted by a restart", now); err != nil {
				return err
			}
		}
	}
	if len(rows) > 0 {
		s.Log.WarnContext(ctx, "runs interrupted by a restart marked failed", "count", len(rows))
	}
	return tx.Commit(ctx)
}

// Loop sweeps and starts runs every tick until ctx ends, then waits for the
// runs in flight to give up.
func (s *Scheduler) Loop(ctx context.Context) {
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		if err := s.Pass(ctx); err != nil && ctx.Err() == nil {
			s.Log.ErrorContext(ctx, "run scheduler", "err", err)
		}
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// Pass times out overdue runs and starts as many queued ones as slots allow.
func (s *Scheduler) Pass(ctx context.Context) error {
	if err := s.timeOut(ctx); err != nil {
		return err
	}
	if err := s.archiveStale(ctx); err != nil {
		return err
	}
	if s.Verification {
		if err := s.enqueueVerification(ctx); err != nil {
			return err
		}
	}
	if s.Runner == nil {
		return nil
	}
	for {
		running, err := s.Store.Q.CountRunning(ctx)
		if err != nil {
			return err
		}
		if int(running) >= s.Slots {
			return nil
		}
		now := s.now()
		r, err := s.Store.Q.ClaimQueuedRun(ctx, sqlcgen.ClaimQueuedRunParams{Now: now, Deadline: now.Add(s.Timeout + deadlineMargin)})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.execute(ctx, r)
		}()
	}
}

// execute runs one claimed run to its end. Shutting down cancels the call to
// the runner, not the writes that record the outcome.
func (s *Scheduler) execute(callCtx context.Context, r sqlcgen.Run) {
	ctx := context.WithoutCancel(callCtx)
	log := s.Log.With("run", r.ID, "kind", r.Kind)
	req, apex, err := s.request(ctx, r)
	if err != nil {
		s.finish(ctx, r, "failed", err.Error(), nil, nil)
		log.ErrorContext(ctx, "run not started", "err", err)
		return
	}
	log.InfoContext(ctx, "run started", "domain", req.Domain, "stage", req.Stages)
	raw, err := s.Runner.Run(callCtx, req)
	switch {
	case errors.Is(err, ErrBusy):
		if err := s.Store.Q.RequeueRun(ctx, r.ID); err != nil {
			log.ErrorContext(ctx, "requeue", "err", err)
		}
		return
	case err != nil:
		s.finish(ctx, r, "failed", err.Error(), nil, nil)
		log.ErrorContext(ctx, "run failed", "err", err)
		return
	}
	sum, err := s.ingest(ctx, r, apex, raw)
	if err != nil {
		s.finish(ctx, r, "failed", "ingestion: "+err.Error(), nil, nil)
		log.ErrorContext(ctx, "ingestion failed", "err", err)
		return
	}
	log.InfoContext(ctx, "run ingested", "summary", sum)
	if s.OnIngested != nil {
		s.OnIngested()
	}
}

// enqueueVerification queues one run over the due hosts of the program holding
// the oldest due date, full before resolve. One verification is queued or
// running at a time.
func (s *Scheduler) enqueueVerification(ctx context.Context) error {
	now := s.now()
	programID, err := s.Store.Q.NextDueProgram(ctx, now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	lim := int32(s.Batch) //nolint:gosec // validated by the config
	stage := "full"
	targets, err := s.Store.Q.DueFull(ctx, sqlcgen.DueFullParams{ProgramID: programID, Now: now, Lim: lim})
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		stage = "resolve"
		if targets, err = s.Store.Q.DueResolve(ctx, sqlcgen.DueResolveParams{ProgramID: programID, Now: now, Lim: lim}); err != nil {
			return err
		}
	}
	if len(targets) == 0 {
		return nil
	}
	_, err = s.Store.Q.EnqueueVerification(ctx, sqlcgen.EnqueueVerificationParams{
		ProgramID: programID, Stage: stage, Targets: targets, Now: now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // one is already queued or running
	}
	return err
}

// archiveStale takes assets inactive or unobservable for too long out of the
// scheduler. They are kept: archived is never deleted.
func (s *Scheduler) archiveStale(ctx context.Context) error {
	if s.ArchiveAfter <= 0 {
		return nil
	}
	now := s.now()
	rows, err := s.Store.Q.ArchiveStale(ctx, sqlcgen.ArchiveStaleParams{Before: now.Add(-s.ArchiveAfter), Now: now})
	if err != nil {
		return err
	}
	for _, r := range rows {
		before, _ := json.Marshal(map[string]string{"lifecycle": r.Lifecycle})
		after, _ := json.Marshal(map[string]string{"lifecycle": "archived"})
		if err := s.Store.Q.InsertChange(ctx, sqlcgen.InsertChangeParams{
			AssetID: r.AssetID, ProgramID: r.ProgramID, Kind: "lifecycle_changed", Before: before, After: after, Now: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) ports() string {
	ports := make([]string, len(s.Ports))
	for i, p := range s.Ports {
		ports[i] = strconv.Itoa(p)
	}
	return strings.Join(ports, ",")
}

func (s *Scheduler) request(ctx context.Context, r sqlcgen.Run) (Request, string, error) {
	if r.Kind == "verification" {
		rules, err := s.Store.Q.ListOpenRules(ctx, r.ProgramID)
		if err != nil {
			return Request{}, "", err
		}
		return Request{
			Targets: r.Targets, Exclude: Exclusions(rules), Stages: r.Stage, Ports: s.ports(), Timeout: s.Timeout.String(),
		}, "", nil
	}
	if r.Kind != "recon" || r.WildcardID == nil {
		return Request{}, "", fmt.Errorf("run kind %q is not handled yet", r.Kind)
	}
	w, err := s.Store.Q.GetWildcard(ctx, *r.WildcardID)
	if err != nil {
		return Request{}, "", err
	}
	if !w.RuleInForce {
		return Request{}, "", errors.New("the wildcard's rule was closed while the run was queued")
	}
	rules, err := s.Store.Q.ListOpenRules(ctx, r.ProgramID)
	if err != nil {
		return Request{}, "", err
	}
	return Request{
		Domain: w.Apex, Exclude: Exclusions(rules), Stages: r.Stage, Ports: s.ports(), Timeout: s.Timeout.String(),
	}, w.Apex, nil
}

// Exclusions are the program's exclude rules that name hosts, in FastRecon's
// syntax. url_prefix and cidr exclusions cannot be pushed down: they are
// applied at ingestion.
func Exclusions(rules []sqlcgen.ScopeRule) []string {
	var out []string
	for _, r := range rules {
		if r.Kind != scope.Exclude {
			continue
		}
		switch r.Matcher {
		case scope.FQDN:
			out = append(out, r.Pattern)
		case scope.Apex:
			out = append(out, r.Pattern, "*."+r.Pattern)
		}
	}
	// Sorted, so the same rules always send the same request.
	slices.Sort(out)
	return out
}

// ingest writes the report, finishes the run and marks the wildcard, in one
// transaction. A run the sweeper already timed out is not ingested.
func (s *Scheduler) ingest(ctx context.Context, r sqlcgen.Run, apex string, raw []byte) (ingest.Summary, error) {
	rep, err := ingest.Parse(raw)
	if err != nil {
		return ingest.Summary{}, err
	}
	want := "domain"
	if r.Kind == "verification" {
		want = "targets"
	}
	if rep.Run.Input != want {
		return ingest.Summary{}, fmt.Errorf("a %s report must have input %s, got %q", r.Kind, want, rep.Run.Input)
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return ingest.Summary{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Store.Q.WithTx(tx)

	now := s.now()
	sum, err := ingest.Ingest(ctx, q, ingest.Input{
		Run: r, Apex: apex, Report: rep, Now: now, Log: s.Log,
		Signatures: s.Signatures, Cadences: s.Cadences, Rand: s.Rand, Ports: s.Ports, GeoIP: s.GeoIP,
	})
	if err != nil {
		return ingest.Summary{}, err
	}
	meta, err := json.Marshal(rep.Meta())
	if err != nil {
		return ingest.Summary{}, err
	}
	sj, err := json.Marshal(sum)
	if err != nil {
		return ingest.Summary{}, err
	}
	n, err := q.FinishRun(ctx, sqlcgen.FinishRunParams{ID: r.ID, State: "succeeded", Now: now, ReportMeta: meta, Ingest: sj})
	if err != nil {
		return ingest.Summary{}, err
	}
	if n == 0 {
		return ingest.Summary{}, errors.New("the run is no longer running (timed out?), report dropped")
	}
	if r.WildcardID != nil {
		if err := q.MarkWildcardRecon(ctx, sqlcgen.MarkWildcardReconParams{ID: *r.WildcardID, RunID: &r.ID, Now: now}); err != nil {
			return ingest.Summary{}, err
		}
	}
	if r.Kind == "recon" {
		title := fmt.Sprintf("*.%s finished: %s hosts, %s new, %s new services",
			apex, notify.Count(sum.Hosts), notify.Count(sum.AssetsCreated), notify.Count(sum.ServicesCreated))
		if err := s.notifyRecon(ctx, q, r.ID, title, "/changes?run="+r.ID.String(), now); err != nil {
			return ingest.Summary{}, err
		}
	}
	return sum, tx.Commit(ctx)
}

// notifyRecon records the message of a recon that ended: you started it, or
// auto-scan did.
func (s *Scheduler) notifyRecon(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID, title, path string, now time.Time) error {
	return notify.Enqueue(ctx, q, notify.Event{
		Kind: notify.KindReconFinished, Group: "run:" + id.String(), Title: title,
		Link: strings.TrimRight(s.PublicURL, "/") + path,
	}, now)
}

// reconEnded writes the message of a recon that ended without a report.
func (s *Scheduler) reconEnded(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID, wildcard *uuid.UUID, what string, now time.Time) error {
	name := "a recon"
	if wildcard != nil {
		if w, err := q.GetWildcard(ctx, *wildcard); err == nil {
			name = "*." + w.Apex
		}
	}
	if r := []rune(what); len(r) > 200 {
		what = string(r[:199]) + "…"
	}
	return s.notifyRecon(ctx, q, id, name+" "+what, "/runs/"+id.String(), now)
}

// finish ends a run that brought no report, with its message when it is a
// recon, in one transaction.
func (s *Scheduler) finish(ctx context.Context, r sqlcgen.Run, state, msg string, meta, sum []byte) {
	err := func() error {
		tx, err := s.Store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q := s.Store.Q.WithTx(tx)
		now := s.now()
		n, err := q.FinishRun(ctx, sqlcgen.FinishRunParams{ID: r.ID, State: state, Now: now, Error: &msg, ReportMeta: meta, Ingest: sum})
		if err != nil {
			return err
		}
		if n > 0 && r.Kind == "recon" {
			if err := s.reconEnded(ctx, q, r.ID, r.WildcardID, "failed: "+msg, now); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		s.Log.ErrorContext(ctx, "finishing run", "run", r.ID, "err", err)
	}
}

// timeOut ends the runs past their deadline, a recon with its message.
func (s *Scheduler) timeOut(ctx context.Context) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Store.Q.WithTx(tx)
	now := s.now()
	rows, err := q.TimeOutRuns(ctx, now)
	if err != nil {
		return err
	}
	for _, r := range rows {
		s.Log.WarnContext(ctx, "run timed out", "run", r.ID)
		if r.Kind == "recon" {
			if err := s.reconEnded(ctx, q, r.ID, r.WildcardID, "timed out", now); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// EnqueueRecon queues a recon on a wildcard. queued is false, with a reason,
// when the wildcard cannot be scanned or already has a run queued or running.
func EnqueueRecon(ctx context.Context, q *sqlcgen.Queries, wildcardID uuid.UUID, trigger string, now time.Time) (run sqlcgen.Run, queued bool, reason string, err error) {
	w, err := q.GetWildcard(ctx, wildcardID)
	if err != nil {
		return sqlcgen.Run{}, false, "", err
	}
	p, err := q.GetProgram(ctx, w.ProgramID)
	if err != nil {
		return sqlcgen.Run{}, false, "", err
	}
	switch {
	case !w.RuleInForce:
		return sqlcgen.Run{}, false, "the wildcard's rule is closed", nil
	case p.State != "active":
		return sqlcgen.Run{}, false, "the program is " + p.State, nil
	}
	r, err := q.EnqueueRun(ctx, sqlcgen.EnqueueRunParams{
		ProgramID: w.ProgramID, Kind: "recon", Stage: "full", WildcardID: &w.ID, Trigger: trigger, Now: now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Run{}, false, "a recon is already queued or running on this wildcard", nil
	}
	if err != nil {
		return sqlcgen.Run{}, false, "", err
	}
	return r, true, "", nil
}
