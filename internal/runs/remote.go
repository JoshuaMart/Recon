package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// watchEvery is how often the jobs' states are read.
const watchEvery = time.Minute

// ErrRunClosed is a run that is no longer running: its report, a second copy
// of one already ingested say, is not taken.
var ErrRunClosed = errors.New("the run is no longer running")

// ErrNotARun is a run that does not exist, or is not a job's.
var ErrNotARun = errors.New("no such job run")

// ErrBadReport is a report Recon refuses: a retry would carry the same one.
var ErrBadReport = errors.New("the report is refused")

// JobArgs is a run's whole invocation, as arguments: they replace the
// definition's, so nothing left on it can pin a run. The tokens travel in
// headers, never in a URL that lands in access logs.
func (s *Scheduler) JobArgs(r sqlcgen.Run, req Request) []string {
	base := strings.TrimRight(s.CallbackURL, "/") + "/runs/" + r.ID.String()
	deadline := s.now().Add(s.Timeout + deadlineMargin)
	if r.Deadline != nil {
		deadline = *r.Deadline
	}
	args := []string{"--stages", req.Stages, "--ports", req.Ports, "--timeout", req.Timeout}
	if req.Domain != "" {
		args = append(args, "-d", req.Domain)
	} else {
		args = append(args,
			"--targets-url", base+"/targets",
			"--targets-header", "Authorization: Bearer "+s.Tokens.Mint(PurposeTargets, r.ID, deadline))
	}
	for _, e := range req.Exclude {
		args = append(args, "--exclude", e)
	}
	return append(args,
		"--webhook-url", base+"/report",
		// Valid past the deadline: a run that spends its whole budget can
		// still deliver.
		"--webhook-header", "Authorization: Bearer "+s.Tokens.Mint(PurposeReport, r.ID, deadline.Add(s.Grace)),
		// Ingesting a large report takes longer than FastRecon's default 30 s:
		// a hang-up would roll the write back while retries redo the work.
		"--webhook-timeout", s.ReportTimeout.String())
}

// redact hides the tokens of an invocation, for the log.
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "Authorization: ") {
			a = "Authorization: [redacted]"
		}
		out[i] = a
	}
	return out
}

// startJob starts the job of a claimed run. The run stays running until its
// report arrives, its job fails, or its deadline passes.
func (s *Scheduler) startJob(callCtx context.Context, r sqlcgen.Run) {
	ctx := context.WithoutCancel(callCtx)
	log := s.Log.With("run", r.ID, "kind", r.Kind)
	req, _, err := s.request(ctx, r)
	if err != nil {
		s.finish(ctx, r, "failed", err.Error(), nil, nil)
		log.ErrorContext(ctx, "run not started", "err", err)
		return
	}
	args := s.JobArgs(r, req)
	id, err := s.Jobs.Start(callCtx, args)
	if err != nil {
		s.finish(ctx, r, "failed", err.Error(), nil, nil)
		log.ErrorContext(ctx, "job not started", "err", err)
		return
	}
	if err := s.Store.Q.SetExternalID(ctx, sqlcgen.SetExternalIDParams{ID: r.ID, ExternalID: &id}); err != nil {
		// Unrecorded, the job could be neither watched nor stopped at its
		// deadline: it is stopped now and the run fails, to be queued again.
		log.ErrorContext(ctx, "recording the job, stopping it", "job", id, "err", err)
		if stopErr := s.Jobs.Stop(ctx, id); stopErr != nil {
			log.ErrorContext(ctx, "stopping an unrecorded job", "job", id, "err", stopErr)
		}
		s.finish(ctx, r, "failed", "the job could not be recorded: "+err.Error(), nil, nil)
		return
	}
	log.InfoContext(ctx, "job started", "job", id, "args", redact(args))
}

// watchJobs reads the state of each running job, once a minute: a job the
// platform calls failed closes its run at once, with the platform's reason,
// instead of at its deadline.
func (s *Scheduler) watchJobs(ctx context.Context) error {
	now := s.now()
	if now.Sub(s.lastWatch) < watchEvery {
		return nil
	}
	s.lastWatch = now
	jobs, err := s.Store.Q.RunningJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		st, err := s.Jobs.State(ctx, *j.ExternalID)
		if err != nil {
			s.Log.WarnContext(ctx, "reading a job", "run", j.ID, "job", *j.ExternalID, "err", err)
			continue
		}
		if !st.Failed {
			// Running, or ended well with its report on the way: the deadline
			// covers a report that never comes.
			continue
		}
		msg := "the job " + st.Reason
		if st.ExitCode != nil {
			msg += fmt.Sprintf(" (exit %d%s)", *st.ExitCode, exitMeaning(*st.ExitCode))
		}
		r, err := s.Store.Q.GetRun(ctx, j.ID)
		if err != nil {
			return err
		}
		s.finish(ctx, r, "failed", msg, nil, nil)
		s.Log.WarnContext(ctx, "job failed", "run", j.ID, "job", *j.ExternalID, "reason", msg)
	}
	return nil
}

// exitMeaning is FastRecon's word for its exit codes.
func exitMeaning(code int) string {
	switch code {
	case 1:
		return ": configuration"
	case 2:
		return ": incomplete"
	case 3:
		return ": delivery failed"
	case 4:
		return ": no report"
	}
	return ""
}

// jobRun reads a run a job asks about: running, and a job's.
func (s *Scheduler) jobRun(ctx context.Context, id uuid.UUID) (sqlcgen.Run, error) {
	r, err := s.Store.Q.GetRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && r.Runner != onScaleway) {
		return sqlcgen.Run{}, ErrNotARun
	}
	if err != nil {
		return sqlcgen.Run{}, err
	}
	if r.State != "running" {
		return sqlcgen.Run{}, ErrRunClosed
	}
	return r, nil
}

// Targets is a running job's target list.
func (s *Scheduler) Targets(ctx context.Context, id uuid.UUID) ([]string, error) {
	r, err := s.jobRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(r.Targets) == 0 {
		return nil, ErrNotARun // a recon enumerates, it has no list
	}
	return r.Targets, nil
}

// Deliver ingests the report a job posted. A report Recon cannot read fails
// the run and answers ErrBadReport: FastRecon does not retry a refusal, and a
// retry would carry the same document.
func (s *Scheduler) Deliver(ctx context.Context, id uuid.UUID, raw []byte) (ingest.Summary, error) {
	r, err := s.jobRun(ctx, id)
	if err != nil {
		return ingest.Summary{}, err
	}
	apex := ""
	if r.WildcardID != nil {
		w, err := s.Store.Q.GetWildcard(ctx, *r.WildcardID)
		if err != nil {
			return ingest.Summary{}, err
		}
		apex = w.Apex
	}
	sum, err := s.ingest(ctx, r, apex, raw)
	if errors.Is(err, ErrBadReport) {
		s.finish(ctx, r, "failed", err.Error(), nil, nil)
		return ingest.Summary{}, err
	}
	if err != nil {
		// ErrRunClosed included: timed out while the report was read, the
		// same answer as a second copy.
		return ingest.Summary{}, err
	}
	s.Log.InfoContext(ctx, "job report ingested", "run", id, "summary", sum)
	if s.OnIngested != nil {
		s.OnIngested()
	}
	return sum, nil
}
