package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Notifier reads events, aggregates and sends.
//
// It is the only asynchronous half. Producing an event is ingestion's job, in
// its transaction; this can lag without consequence, which is why it is allowed
// to be a loop at all.
type Notifier struct {
	pool   *pgxpool.Pool
	sender *Sender
	batch  int
	// tips remembers the last mass tip per programme, which is the one piece
	// of state a window may hold in memory: it is a cooldown rather than a
	// count, so a restart re-emitting once is the safe direction.
	tips map[uuid.UUID]tip
	now  func() time.Time
	log  *slog.Logger
}

type tip struct {
	tier int
	at   time.Time
}

// New builds the notifier.
func New(pool *pgxpool.Pool, sender *Sender, batch int, log *slog.Logger) *Notifier {
	if batch <= 0 {
		batch = 200
	}
	return &Notifier{
		pool: pool, sender: sender, batch: batch,
		tips: map[uuid.UUID]tip{}, now: time.Now, log: log,
	}
}

// Summary is what one tick did.
type Summary struct {
	Read       int
	Sent       int
	Suppressed int
	Summarised int
	Failed     int
	Nowhere    int
}

// Once drains what is pending.
func (n *Notifier) Once(ctx context.Context) (Summary, error) {
	var summary Summary
	queries := sqlcgen.New(n.pool)

	pending, err := queries.PendingEvents(ctx, sqlcgen.PendingEventsParams{Batch: bounded(n.batch)})
	if err != nil {
		return summary, fmt.Errorf("read the queue: %w", err)
	}
	summary.Read = len(pending)
	if len(pending) == 0 {
		return summary, nil
	}

	channels := map[uuid.UUID][]Channel{}
	overflowed := map[overflow]Window{}
	// A channel that failed once in this tick is not asked again. With a two
	// hundred event batch and a ten second timeout, a dead webhook otherwise
	// makes one tick take half an hour against a thirty second interval.
	broken := map[uuid.UUID]struct{}{}
	for _, event := range pending {
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		org := uuid.UUID(event.OrgID.Bytes)
		if _, read := channels[org]; !read {
			found, err := n.channels(ctx, queries, org)
			if err != nil {
				return summary, err
			}
			channels[org] = found
		}
		n.deliver(ctx, queries, event, channels[org], &summary, overflowed, broken)
	}

	// Once the whole batch is known, the summaries speak for what they actually
	// stand for.
	for window, span := range overflowed {
		n.summarise(ctx, queries, window, span, &summary)
	}
	return summary, nil
}

// summarise writes the one summary a saturated window owes.
//
// It carries the priority of the window it replaces rather than its own, so a
// flood of high events is summarised at high and reaches a channel whose floor
// would have refused a medium one. And it is written once per window: without
// that, every suppressed event writes its own summary and the anti-flood
// produces one notification per notification it suppressed.
func (n *Notifier) summarise(
	ctx context.Context, q *sqlcgen.Queries, window overflow, span Window, summary *Summary,
) {
	since := stamp(n.now().Add(-span.Span))

	existing, err := q.DigestInWindow(ctx, sqlcgen.DigestInWindowParams{
		ProgramID: window.program, Priority: window.priority, Since: since,
	})
	if err != nil {
		n.log.ErrorContext(ctx, "overflow summary check failed", "program", window.program, "error", err)
		return
	}
	if existing > 0 {
		return
	}

	held, err := q.SuppressedInWindow(ctx, sqlcgen.SuppressedInWindowParams{
		ProgramID: window.program, Priority: window.priority, Since: since,
	})
	if err != nil {
		n.log.ErrorContext(ctx, "overflow count failed", "program", window.program, "error", err)
		return
	}

	batch, err := Batch(uuid.UUID(window.org.Bytes), uuid.UUID(window.program.Bytes), n.now(), []Event{{
		Kind:     KindDigest,
		Priority: window.priority,
		Payload: map[string]any{
			"summary": fmt.Sprintf("%d further %s events on %s in the last %s",
				held, window.kind, window.name, span.Span),
			"held":   held,
			"reason": "window cap",
			"kind":   window.kind,
		},
	}})
	if err != nil {
		n.log.ErrorContext(ctx, "overflow payload failed", "program", window.program, "error", err)
		return
	}

	if _, err := q.WriteEvents(ctx, batch); err != nil {
		n.log.ErrorContext(ctx, "overflow summary failed", "program", window.program, "error", err)
		return
	}
	summary.Summarised++
}

func (n *Notifier) channels(ctx context.Context, q *sqlcgen.Queries, org uuid.UUID) ([]Channel, error) {
	rows, err := q.ChannelsForOrg(ctx, sqlcgen.ChannelsForOrgParams{OrgID: pgUUID(org)})
	if err != nil {
		return nil, fmt.Errorf("read the channels of %s: %w", org, err)
	}
	out := make([]Channel, 0, len(rows))
	for _, row := range rows {
		channel := Channel{
			ID:          uuid.UUID(row.ID.Bytes),
			URL:         row.Url,
			MinPriority: row.MinPriority,
			ManagedBy:   row.ManagedBy,
		}
		if row.SecretRef != nil {
			channel.SecretRef = *row.SecretRef
		}
		if row.Template != nil {
			channel.Template = *row.Template
		}
		out = append(out, channel)
	}
	return out, nil
}

// deliver sends one event, or decides it should not be sent.
// overflow names a window that saturated, so one summary can speak for it.
type overflow struct {
	org      pgtype.UUID
	program  pgtype.UUID
	priority string
	kind     string
	name     string
}

func (n *Notifier) deliver(
	ctx context.Context, q *sqlcgen.Queries, event sqlcgen.PendingEventsRow,
	channels []Channel, summary *Summary, overflowed map[overflow]Window, broken map[uuid.UUID]struct{},
) {
	program := uuid.UUID(event.ProgramID.Bytes)

	// The window is read from the table, never from memory. An in-memory
	// counter resets on restart, which reopens the tap exactly when one
	// restarts because of an incident.
	if Windowed(event.Priority, event.AssetID.Valid) {
		window := Windows[event.Priority]
		sent, err := q.CountWindow(ctx, sqlcgen.CountWindowParams{
			ProgramID: event.ProgramID,
			Priority:  event.Priority,
			Since:     stamp(n.now().Add(-window.Span)),
		})
		if err != nil {
			n.log.ErrorContext(ctx, "window count failed", "program", program, "error", err)
			summary.Failed++
			return
		}
		if sent >= int64(window.Cap) {
			// Past the cap the individual event stays in the database,
			// readable and not sent, and a summary speaks for it. An overflow
			// must never produce the absence of a notification.
			if _, err := q.SuppressWindow(ctx, sqlcgen.SuppressWindowParams{
				Ids:        []int64{event.ID},
				CreatedAts: []pgtype.Timestamptz{event.CreatedAt},
			}); err != nil {
				n.log.ErrorContext(ctx, "suppression failed", "event", event.ID, "error", err)
				summary.Failed++
				return
			}
			summary.Suppressed++
			// The summary is written once the tick is over, not here. Written
			// on the first event past the cap it would count one held event and
			// claim to speak for the four thousand nine hundred that follow.
			overflowed[overflow{
				org: event.OrgID, program: event.ProgramID,
				priority: event.Priority, kind: event.Kind, name: event.ProgramName,
			}] = window
			return
		}
	}

	message := n.message(event)

	// An organization with no channel is a deliberate configuration: the event
	// is marked delivered and counted, so "computed and sent nowhere" stays
	// visible rather than looking like a queue that is draining.
	eligible := 0
	for _, channel := range channels {
		if AtLeast(event.Priority, channel.MinPriority) {
			eligible++
		}
	}
	if eligible == 0 {
		summary.Nowhere++
		n.mark(ctx, q, event, summary)
		return
	}

	for _, channel := range channels {
		if !AtLeast(event.Priority, channel.MinPriority) {
			continue
		}
		if _, dead := broken[channel.ID]; dead {
			summary.Failed++
			return
		}
		if err := n.sender.Send(ctx, channel, message); err != nil {
			broken[channel.ID] = struct{}{}
			// The event stays queued. A dead webhook is an observability
			// outage rather than a transport detail, and the stuck queue
			// alert is what says so.
			n.log.ErrorContext(ctx, "notification not delivered",
				"event", event.ID, "kind", event.Kind, "channel", channel.ID, "error", err)
			summary.Failed++
			return
		}
	}

	summary.Sent++
	n.mark(ctx, q, event, summary)
}

func (n *Notifier) mark(ctx context.Context, q *sqlcgen.Queries, event sqlcgen.PendingEventsRow, summary *Summary) {
	// By (id, created_at) rather than id alone: the primary key is composite,
	// and forgetting the second half scans every partition on each mark.
	if err := q.MarkNotified(ctx, sqlcgen.MarkNotifiedParams{
		ID:        event.ID,
		CreatedAt: event.CreatedAt,
		At:        stamp(n.now()),
	}); err != nil {
		n.log.ErrorContext(ctx, "mark failed", "event", event.ID, "error", err)
		summary.Failed++
	}
}

// message renders what a channel is told.
func (n *Notifier) message(event sqlcgen.PendingEventsRow) Message {
	var payload map[string]any
	_ = json.Unmarshal(event.Payload, &payload)

	message := Message{
		Kind:      event.Kind,
		Priority:  event.Priority,
		Program:   event.ProgramName,
		CreatedAt: event.CreatedAt.Time,
		Payload:   payload,
	}
	if event.AssetKey != nil {
		message.Asset = *event.AssetKey
	}
	if line, ok := payload["summary"].(string); ok {
		message.Summary = line
	}
	message.Text = Line(message)
	return message
}

// The diff is in the payload for a template that wants it; this is what somebody
// reads on a phone, and it has to say which asset and what moved rather than
// which event kind fired.
// Line is the sentence a notification leads with.
func Line(message Message) string {
	subject := message.Asset
	if subject == "" {
		subject = message.Program
	}

	switch message.Kind {
	case KindTakeover:
		finding, _ := message.Payload["finding"].(map[string]any)
		return fmt.Sprintf("Takeover candidate: %s points at %v (%v)",
			subject, finding["target"], finding["signature"])
	case KindNewActive:
		return fmt.Sprintf("New active asset on %s: %s", message.Program, subject)
	case KindPortOpened:
		return fmt.Sprintf("New open port on %s: %s", subject, message.Summary)
	case KindWentInactive:
		return fmt.Sprintf("%s went inactive", subject)
	case KindUnobservable:
		return fmt.Sprintf("%s has gone dark: %s", message.Program, message.Summary)
	case KindRunNeverEnded:
		return fmt.Sprintf("%s: the first discovery run never completed", message.Program)
	case KindDigest:
		return fmt.Sprintf("%s: %s", message.Program, message.Summary)
	case KindDetection:
		return fmt.Sprintf("%s: detection improved, %s", subject, message.Summary)
	default:
		return fmt.Sprintf("%s changed on %s: %s", field(message.Kind), subject, message.Summary)
	}
}

// field turns an event kind into the noun a sentence starts with.
func field(kind string) string {
	switch kind {
	case KindTechChanged:
		return "Technologies"
	case KindChainChanged:
		return "The redirect chain"
	case KindCertChanged:
		return "The certificate"
	case KindTitleChanged:
		return "The title"
	default:
		return kind
	}
}

// bounded narrows a batch size to the column that holds it.
func bounded(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < 1 {
		return 1
	}
	return int32(n)
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}
