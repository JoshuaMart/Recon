package inventory

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Settle is the end of a pass on one asset: the layers take the outcomes, the
// lifecycle takes the verdict, and a host takes its next due dates.
type Settle struct {
	ID   uuid.UUID
	Kind string
	// Outcomes are this pass's outcomes per layer. Empty for a host the run
	// listed and never reached: nothing is counted, only scheduling moves.
	Outcomes map[string]string
	// Verify is true for a verification run, the only kind allowed to count a
	// failure. A recon only resets and promotes.
	Verify bool
	Stage  string // resolve or full
	// New is set when the pass created the asset, which then needs its first
	// due dates. A recon leaves the dates of an asset it did not move alone:
	// it is verification's to pace.
	New bool
}

// Settle applies one pass. It returns the lifecycle before and after.
func (w *Writer) Settle(ctx context.Context, s Settle) (before, after string, err error) {
	cur, err := w.Q.GetAssetCurrent(ctx, s.ID)
	if err != nil {
		return "", "", err
	}
	rows, err := w.Q.GetLayers(ctx, s.ID)
	if err != nil {
		return "", "", err
	}
	layers := make(map[string]lifecycle.Layer, len(rows))
	for _, r := range rows {
		layers[r.Layer] = lifecycle.Layer{
			Informative: int(r.InformativeFailures), NonInformative: int(r.NonInformativeFailures),
			FirstFailure: r.FirstFailureAt, LastOK: r.LastOkAt,
		}
	}
	okNow := false
	for layer, outcome := range s.Outcomes {
		l := lifecycle.Apply(layers[layer], outcome, w.Now, s.Verify)
		layers[layer] = l
		okNow = okNow || outcome == lifecycle.OK
		if err := w.Q.PutLayer(ctx, sqlcgen.PutLayerParams{
			AssetID: s.ID, Layer: layer,
			InformativeFailures: int32(l.Informative), NonInformativeFailures: int32(l.NonInformative), //nolint:gosec // small counters
			FirstFailureAt: l.FirstFailure, LastOkAt: l.LastOK, Now: w.Now,
		}); err != nil {
			return "", "", err
		}
	}

	before = cur.Lifecycle
	after = lifecycle.Verdict(s.Kind, before, layers, okNow, w.Now)
	// A recon never moves an asset toward a death: it only brings back what
	// answers.
	revived := after == lifecycle.Active && (before == lifecycle.Candidate || before == lifecycle.Archived)
	if !s.Verify && !revived {
		after = before
	}
	if after != before {
		if err := w.SetLifecycle(ctx, s.ID, after); err != nil {
			return "", "", err
		}
	}

	if s.Kind != KindFQDN || (!s.Verify && !s.New && before == after) {
		return before, after, nil
	}
	if cur.ScopeStatus != "in_scope" || !w.ProgramActive {
		return before, after, w.Q.SetSchedule(ctx, sqlcgen.SetScheduleParams{AssetID: s.ID, CheckedAt: &w.Now})
	}
	failing := map[string]bool{}
	for name, l := range layers {
		if l.Informative > 0 {
			failing[name] = true
		}
	}
	if s.Verify && len(s.Outcomes) == 0 && !s.New {
		// Listed and not reached: nothing was measured, but the date of the
		// stage that ran must move, or the host is due again at once.
		res, full := w.cadences().Retry(s.Stage, cur.NextResolveAt, cur.NextFullAt, w.Now, w.rand())
		return before, after, w.Q.SetSchedule(ctx, sqlcgen.SetScheduleParams{
			AssetID: s.ID, NextResolveAt: res, NextFullAt: full, BackoffTier: cur.BackoffTier,
		})
	}
	tier := int(cur.BackoffTier)
	if before != after {
		tier = 0
	}
	plan := w.cadences().Schedule(lifecycle.Input{
		State: after, Previous: before, Stage: s.Stage, Tier: tier, Failing: failing, FirstSeen: cur.FirstSeen,
		NextResolve: cur.NextResolveAt, NextFull: cur.NextFullAt,
	}, w.Now, w.rand())
	if plan.Archive {
		if err := w.SetLifecycle(ctx, s.ID, lifecycle.Archived); err != nil {
			return "", "", err
		}
		after = lifecycle.Archived
	}
	return before, after, w.Q.SetSchedule(ctx, sqlcgen.SetScheduleParams{
		AssetID: s.ID, NextResolveAt: plan.NextResolve, NextFullAt: plan.NextFull,
		BackoffTier: int32(plan.Tier), CheckedAt: &w.Now, //nolint:gosec // bounded by the curve length
	})
}

func (w *Writer) cadences() lifecycle.Cadences {
	if w.Cadences == nil {
		return lifecycle.DefaultCadences
	}
	return *w.Cadences
}

func (w *Writer) rand() func() float64 {
	if w.Rand == nil {
		return rand.Float64 //nolint:gosec // jitter, not a secret
	}
	return w.Rand
}

// SyncSchedule makes a program's due dates follow its scope and state: none
// outside the perimeter or on an inactive program, and a resolve soon for an
// in-scope host that has none.
func SyncSchedule(ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID, now time.Time) error {
	if _, err := q.ClearProgramSchedule(ctx, programID); err != nil {
		return err
	}
	_, err := q.FillProgramSchedule(ctx, sqlcgen.FillProgramScheduleParams{ID: programID, Now: now, JitterSeconds: (15 * time.Minute).Seconds()})
	return err
}
