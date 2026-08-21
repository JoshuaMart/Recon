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
		n.deliver(ctx, queries, event, channels[org], &summary)
	}
	return summary, nil
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
func (n *Notifier) deliver(
	ctx context.Context, q *sqlcgen.Queries, event sqlcgen.PendingEventsRow,
	channels []Channel, summary *Summary,
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
		if err := n.sender.Send(ctx, channel, message); err != nil {
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
		return fmt.Sprintf("Takeover candidate on %s: %v points at %v (%v)",
			subject, subject, finding["target"], finding["signature"])
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

// jsonPayload encodes what an event carries.
func jsonPayload(payload map[string]any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	return encoded, nil
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
