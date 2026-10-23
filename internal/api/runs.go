package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

type runView struct {
	ID         uuid.UUID       `json:"id"`
	ProgramID  uuid.UUID       `json:"program_id"`
	Kind       string          `json:"kind"`
	Stage      string          `json:"stage"`
	WildcardID *uuid.UUID      `json:"wildcard_id"`
	Apex       *string         `json:"apex,omitempty"`
	State      string          `json:"state"`
	Trigger    string          `json:"trigger"`
	QueuedAt   time.Time       `json:"queued_at"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Error      *string         `json:"error"`
	Report     json.RawMessage `json:"report,omitempty"`
	Ingest     json.RawMessage `json:"ingest,omitempty"`
}

func viewRun(r sqlcgen.Run) runView {
	return runView{
		ID: r.ID, ProgramID: r.ProgramID, Kind: r.Kind, Stage: r.Stage, WildcardID: r.WildcardID, State: r.State,
		Trigger: r.Trigger, QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Error: r.Error,
		Report: raw(r.ReportMeta), Ingest: raw(r.Ingest),
	}
}

func raw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return b
}

type queuedView struct {
	WildcardID uuid.UUID  `json:"wildcard_id"`
	Queued     bool       `json:"queued"`
	Reason     string     `json:"reason,omitempty"`
	RunID      *uuid.UUID `json:"run_id,omitempty"`
}

func (s *server) reconWildcard(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.inTx(w, r, func(q *sqlcgen.Queries) (int, any, error) {
		run, queued, reason, err := runs.EnqueueRecon(r.Context(), q, id, "user", s.now())
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "id not found")
			return 0, nil, errHandled
		}
		if err != nil {
			return 0, nil, err
		}
		v := queuedView{WildcardID: id, Queued: queued, Reason: reason}
		if queued {
			v.RunID = &run.ID
		}
		return http.StatusOK, v, nil
	})
}

// maxOldest bounds "scan the N oldest".
const maxOldest = 500

func (s *server) reconOldest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Oldest    int        `json:"oldest"`
		ProgramID *uuid.UUID `json:"program_id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Oldest < 1 || body.Oldest > maxOldest {
		writeError(w, http.StatusBadRequest, "bad_field", "oldest is 1 to "+strconv.Itoa(maxOldest))
		return
	}
	s.inTx(w, r, func(q *sqlcgen.Queries) (int, any, error) {
		ws, err := q.OldestWildcards(r.Context(), sqlcgen.OldestWildcardsParams{ProgramID: body.ProgramID, Lim: int32(body.Oldest)}) //nolint:gosec // bounded above
		if err != nil {
			return 0, nil, err
		}
		out := make([]queuedView, 0, len(ws))
		for _, wc := range ws {
			run, queued, reason, err := runs.EnqueueRecon(r.Context(), q, wc.ID, "user", s.now())
			if err != nil {
				return 0, nil, err
			}
			v := queuedView{WildcardID: wc.ID, Queued: queued, Reason: reason}
			if queued {
				v.RunID = &run.ID
			}
			out = append(out, v)
		}
		return http.StatusOK, map[string]any{"wildcards": out}, nil
	})
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	params := sqlcgen.ListRunsParams{Lim: 100}
	if v := r.URL.Query().Get("program"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_field", "program is not an id")
			return
		}
		params.ProgramID = &id
	}
	rows, err := s.Store.Q.ListRuns(r.Context(), params)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]runView, 0, len(rows))
	for _, row := range rows {
		v := viewRun(row.Run)
		v.Report = nil // the list carries ingestion counters, the run page the full report
		v.Apex = row.Apex
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	row, err := s.Store.Q.GetRun(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "id not found")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewRun(row))
}
