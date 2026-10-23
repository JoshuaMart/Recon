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

	"github.com/JoshuaMart/recon/internal/ingest"
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

	wg sync.WaitGroup
}

func (s *Scheduler) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

// Recover fails the runs a stopped process left running: a synchronous call
// does not survive its caller.
func (s *Scheduler) Recover(ctx context.Context) error {
	n, err := s.Store.Q.FailInterruptedRuns(ctx, s.now())
	if err == nil && n > 0 {
		s.Log.WarnContext(ctx, "runs interrupted by a restart marked failed", "count", n)
	}
	return err
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
	ids, err := s.Store.Q.TimeOutRuns(ctx, s.now())
	if err != nil {
		return err
	}
	for _, id := range ids {
		s.Log.WarnContext(ctx, "run timed out", "run", id)
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
			s.execute(context.WithoutCancel(ctx), r)
		}()
	}
}

// Execute runs one claimed run to its end: used by Pass, and by tests.
func (s *Scheduler) execute(ctx context.Context, r sqlcgen.Run) {
	log := s.Log.With("run", r.ID, "kind", r.Kind)
	req, apex, err := s.request(ctx, r)
	if err != nil {
		s.finish(ctx, r.ID, "failed", err.Error(), nil, nil)
		log.ErrorContext(ctx, "run not started", "err", err)
		return
	}
	log.InfoContext(ctx, "run started", "domain", req.Domain, "stage", req.Stages)
	raw, err := s.Runner.Run(ctx, req)
	switch {
	case errors.Is(err, ErrBusy):
		if err := s.Store.Q.RequeueRun(ctx, r.ID); err != nil {
			log.ErrorContext(ctx, "requeue", "err", err)
		}
		return
	case err != nil:
		s.finish(ctx, r.ID, "failed", err.Error(), nil, nil)
		log.ErrorContext(ctx, "run failed", "err", err)
		return
	}
	sum, err := s.ingest(ctx, r, apex, raw)
	if err != nil {
		s.finish(ctx, r.ID, "failed", "ingestion: "+err.Error(), nil, nil)
		log.ErrorContext(ctx, "ingestion failed", "err", err)
		return
	}
	log.InfoContext(ctx, "run ingested", "summary", sum)
}

func (s *Scheduler) request(ctx context.Context, r sqlcgen.Run) (Request, string, error) {
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
	ports := make([]string, len(s.Ports))
	for i, p := range s.Ports {
		ports[i] = strconv.Itoa(p)
	}
	return Request{
		Domain: w.Apex, Exclude: Exclusions(rules), Stages: r.Stage,
		Ports: strings.Join(ports, ","), Timeout: s.Timeout.String(),
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
	if rep.Run.Input != "domain" {
		return ingest.Summary{}, fmt.Errorf("a recon report must have input domain, got %q", rep.Run.Input)
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return ingest.Summary{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Store.Q.WithTx(tx)

	now := s.now()
	sum, err := ingest.Ingest(ctx, q, ingest.Input{Run: r, Apex: apex, Report: rep, Now: now, Log: s.Log})
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
	if err := q.MarkWildcardRecon(ctx, sqlcgen.MarkWildcardReconParams{ID: *r.WildcardID, RunID: &r.ID, Now: now}); err != nil {
		return ingest.Summary{}, err
	}
	return sum, tx.Commit(ctx)
}

func (s *Scheduler) finish(ctx context.Context, id uuid.UUID, state, msg string, meta, sum []byte) {
	if _, err := s.Store.Q.FinishRun(ctx, sqlcgen.FinishRunParams{
		ID: id, State: state, Now: s.now(), Error: &msg, ReportMeta: meta, Ingest: sum,
	}); err != nil {
		s.Log.ErrorContext(ctx, "finishing run", "run", id, "err", err)
	}
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
