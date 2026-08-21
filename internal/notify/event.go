// Package notify decides what is worth telling somebody, and sends it.
//
// The event is produced inside the ingestion transaction, by ingestion itself.
// A sweeper would have to re-derive what ingestion just computed, the
// transition, the failure qualification, the version classification and the
// comparison of two payloads, and it would miss every transient state on the
// way: an asset going active, flapping and active again between two passes has
// changed nothing from a sweeper's point of view, and a flap is itself a
// signal.
//
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

// Kinds of event. Text with a named check rather than an enum, because the list
// is still moving.
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

// Priorities, and the ordering matters: a channel says which floor it wants.
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

// Batch turns the events of one organization and one programme into the
// parallel arrays the insert takes.
//
// One builder rather than one per call site, and the reason is the shape of the
// statement rather than tidiness: it indexes the arrays together, so a batch
// assembled from two places with one column missing would write nulls instead
// of failing. Built in a single loop, there is no way to produce that.
//
// The organization and the programme are scalars because every caller has them
// as constants of the batch, which is also what makes a batch mixing two
// tenants inexpressible.
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
		// Null rather than a zero uuid on a programme event. That nullability
		// is a rule the database enforces, and a column looking populated while
		// naming nothing is worse than an empty one.
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

// Event is one thing worth telling somebody.
//
// The payload is frozen at write time. A notification reflects the state at the
// moment of the event and not at the moment of sending: a notifier ten minutes
// behind rereading the projection would describe something other than what it
// announces.
type Event struct {
	OrgID     uuid.UUID
	ProgramID uuid.UUID
	// AssetID is nil on a program event, and that nullability is a rule the
	// database enforces rather than a permission.
	AssetID  *uuid.UUID
	Kind     string
	Priority string
	Payload  map[string]any
	// Suppressed is decided here, at write time, and never at drain time. A
	// notifier deciding later would send a first run's flood late rather than
	// never, the grace having ended between the write and the send.
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
