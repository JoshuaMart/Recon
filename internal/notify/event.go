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
	"github.com/google/uuid"
)

// Kinds of event. Text with a named check rather than an enum, because the list
// is still moving.
const (
	KindTakeover      = "takeover_candidate"
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
