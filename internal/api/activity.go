package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// activity is what is happening now: the runs in flight and queued, the
// renders, the CT feed, the sync, and what is failing. Behind the session,
// unlike /api/health, since it says why.
func (s *server) activity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := s.Store.Q
	out := map[string]any{"now": s.now()}

	runRows, err := q.ActiveRuns(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if runRows == nil {
		runRows = []sqlcgen.ActiveRunsRow{}
	}
	queued, err := q.QueuedRunCount(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out["runs"], out["queued"] = runRows, queued

	renders := map[string]any{"state": "disabled"}
	if s.Render != nil {
		st := s.Render()
		renders["state"], renders["in_flight"], renders["limit"] = "unreachable", st.InFlight, st.Limit
		if st.Healthy {
			renders["state"] = "ok"
		}
		_, depth := s.renderHealth(ctx)
		renders["queue"] = depth
	}
	out["renders"] = renders

	ct := map[string]any{"state": "disabled"}
	if s.CT != nil {
		ct["state"] = "disconnected"
		if s.CT() {
			ct["state"] = "ok"
		}
		if sess, err := q.LastCTSession(ctx); err == nil {
			ct["session"] = sess
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.internal(w, r, err)
			return
		}
	}
	out["ct"] = ct

	sync := map[string]any{"state": s.syncHealth(ctx)}
	if st, err := q.GetSyncState(ctx); err == nil {
		sync["last_started_at"], sync["last_success_at"] = st.LastStartedAt, st.LastSuccessAt
		if st.LastErrorAt != nil && (st.LastSuccessAt == nil || st.LastErrorAt.After(*st.LastSuccessAt)) {
			sync["last_error"], sync["last_error_at"] = st.LastError, st.LastErrorAt
		}
	}
	out["sync"] = sync

	problems, err := q.FailingComponents(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if problems == nil {
		problems = []sqlcgen.HealthState{}
	}
	out["problems"] = problems
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
