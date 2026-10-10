// Package notify decides what is worth telling somebody, and sends it. The event is produced
// by ingestion inside its transaction: a sweeper would have to re-derive what ingestion just
// computed (transition, failure qualification, version classification, payload comparison)
// and would miss every transient state, since an asset going active, flapping and active
// again between two passes has changed nothing to a sweeper, and a flap is itself a signal.
// Only the sending is asynchronous.
package notify

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Kinds of event. Text with a named check, not an enum, since the list is moving.
const (
	KindTakeover      = "takeover_candidate"
	KindExternalDead  = "external_host_dead"
	KindNewActive     = "new_active"
	KindPortOpened    = "port_opened"
	KindWentInactive  = "went_inactive"
	KindTechChanged   = "tech_changed"
	KindChainChanged  = "chain_changed"
	KindCertChanged   = "cert_changed"
	KindTitleChanged  = "title_changed"
	KindDetection     = "detection_improved"
	KindUnobservable  = "program_unobservable"
	KindRunNeverEnded = "run_never_completed"
	KindDigest        = "digest"
)

// Priorities. Order matters: a channel says which floor it wants.
const (
	Critical = "critical"
	High     = "high"
	Medium   = "medium"
	Low      = "low"
)

// rank orders the priorities so a floor can be compared.
var rank = map[string]int{Low: 0, Medium: 1, High: 2, Critical: 3}

// AtLeast reports whether a priority clears a channel's floor.
func AtLeast(priority, floor string) bool { return rank[priority] >= rank[floor] }

// Priorities is the table of what each kind is worth.
var Priorities = map[string]string{
	KindTakeover:      Critical,
	KindExternalDead:  Critical,
	KindNewActive:     High,
	KindPortOpened:    High,
	KindUnobservable:  High,
	KindRunNeverEnded: High,
	KindTechChanged:   Medium,
	KindChainChanged:  Medium,
	KindWentInactive:  Medium,
	KindCertChanged:   Low,
	KindTitleChanged:  Low,
	KindDetection:     Low,
	KindDigest:        Medium,
}

// Batch turns one organization's and programme's events into the parallel arrays
// the insert takes. One builder because the statement indexes the arrays together:
// a batch assembled from two places with a column missing would write nulls
// instead of failing. The organization and programme are scalars, which makes a
// batch mixing two tenants inexpressible.
func Batch(org, program uuid.UUID, at time.Time, events []Event) (sqlcgen.WriteEventsParams, error) {
	params := sqlcgen.WriteEventsParams{
		OrgID:      pgtype.UUID{Bytes: org, Valid: true},
		ProgramID:  pgtype.UUID{Bytes: program, Valid: true},
		AssetIds:   make([]pgtype.UUID, 0, len(events)),
		Kinds:      make([]string, 0, len(events)),
		Priorities: make([]string, 0, len(events)),
		Payloads:   make([][]byte, 0, len(events)),
		CreatedAts: make([]pgtype.Timestamptz, 0, len(events)),
		Suppressed: make([]bool, 0, len(events)),
	}
	stamped := pgtype.Timestamptz{Time: at.UTC(), Valid: true}

	for _, event := range events {
		payload, err := json.Marshal(event.Payload)
		if err != nil {
			return sqlcgen.WriteEventsParams{}, fmt.Errorf("encode %s payload: %w", event.Kind, err)
		}
		// Null, not a zero uuid, on a programme event: a database-enforced rule, and a
		// populated-looking column naming nothing is worse than an empty one.
		asset := pgtype.UUID{}
		if event.AssetID != nil {
			asset = pgtype.UUID{Bytes: *event.AssetID, Valid: true}
		}
		params.AssetIds = append(params.AssetIds, asset)
		params.Kinds = append(params.Kinds, event.Kind)
		params.Priorities = append(params.Priorities, event.Priority)
		params.Payloads = append(params.Payloads, payload)
		params.CreatedAts = append(params.CreatedAts, stamped)
		params.Suppressed = append(params.Suppressed, event.Suppressed)
	}
	return params, nil
}

// Event is one thing worth telling somebody. The payload is frozen at write time:
// a notification reflects the moment of the event, and a notifier ten minutes
// behind rereading the projection would describe something else.
type Event struct {
	OrgID     uuid.UUID
	ProgramID uuid.UUID
	// AssetID is nil on a program event, a nullability the database enforces.
	AssetID  *uuid.UUID
	Kind     string
	Priority string
	Payload  map[string]any
	// Suppressed is decided at write time, never drain time: a later decision would
	// send a first run's flood late, the grace having ended between write and send.
	Suppressed bool
}

// ProgramEvent builds one that no asset carries.
func ProgramEvent(org, program uuid.UUID, kind string, payload map[string]any) Event {
	return Event{
		OrgID:     org,
		ProgramID: program,
		Kind:      kind,
		Priority:  Priorities[kind],
		Payload:   payload,
	}
}
