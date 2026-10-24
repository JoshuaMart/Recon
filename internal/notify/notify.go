// Package notify turns a few events into messages: a recon that ended, an
// alerting saved search with new matches, a component broken or back. Events
// join a trailing window per group and leave through the configured channels.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// The kinds of notification, as the notification table names them.
const (
	KindReconFinished = "recon_finished"
	KindSavedSearch   = "saved_search"
	KindBroken        = "broken"
	KindRecovered     = "recovered"
	KindCTLive        = "ct_live"
)

// CTWindow is how long a program's first name from a certificate serving
// HTTP waits for the next ones.
const CTWindow = 5 * time.Minute

// SavedSearchWindow is how long a saved search's first new match waits for
// the next ones.
const SavedSearchWindow = 15 * time.Minute

// MaxItems is how many items a message lists before "and N more".
const MaxItems = 10

// Event is one thing worth a message. Events of one group arriving while its
// notification is not due join it.
type Event struct {
	Kind  string
	Group string
	// Title is the message's first line; for a saved search it is the
	// search's name, and the count is added when the message is written.
	Title string
	Items []string
	// Count is how many things the event adds, at least len(Items).
	Count  int
	Link   string
	Window time.Duration
}

// Enqueue records an event, in the caller's transaction: the notification is
// written with what caused it, or not at all.
func Enqueue(ctx context.Context, q *sqlcgen.Queries, e Event, now time.Time) error {
	n := max(e.Count, len(e.Items))
	items := e.Items
	if items == nil {
		items = []string{}
	}
	if e.Window > 0 {
		id, err := q.OpenNotification(ctx, sqlcgen.OpenNotificationParams{GroupKey: e.Group, Now: now})
		switch {
		case err == nil:
			return q.AppendNotification(ctx, sqlcgen.AppendNotificationParams{ID: id, Items: items, Cap: MaxItems, N: int32(n), Title: e.Title}) //nolint:gosec // a count of assets
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
	}
	var link *string
	if e.Link != "" {
		link = &e.Link
	}
	return q.InsertNotification(ctx, sqlcgen.InsertNotificationParams{
		Kind: e.Kind, GroupKey: e.Group, Title: e.Title, Items: items, Cap: MaxItems, N: int32(n), //nolint:gosec // a count of assets
		Link: link, Now: now, Due: now.Add(e.Window),
	})
}

// Message is a notification ready to send.
type Message struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Items []string `json:"items"`
	// More is how many items the message does not list.
	More int    `json:"more"`
	Link string `json:"link,omitempty"`
	Text string `json:"text"`
}

// Compose writes the message of a notification.
func Compose(n sqlcgen.Notification) Message {
	m := Message{Kind: n.Kind, Title: n.Title, Items: n.Items, More: max(0, int(n.Total)-len(n.Items))}
	if m.Items == nil {
		m.Items = []string{}
	}
	switch n.Kind {
	case KindSavedSearch:
		m.Title = fmt.Sprintf("saved search %q: %d new match%s", n.Title, n.Total, plural(int(n.Total), "es"))
	case KindCTLive:
		if n.Total == 1 {
			m.Title = n.Title + ": a name from a certificate now serves HTTP"
		} else {
			m.Title = fmt.Sprintf("%s: %d names from certificates now serve HTTP", n.Title, n.Total)
		}
	}
	if n.Link != nil {
		m.Link = *n.Link
	}
	var b strings.Builder
	b.WriteString("[recon] " + m.Title)
	for _, it := range m.Items {
		b.WriteString("\n        " + it)
	}
	if m.More > 0 {
		fmt.Fprintf(&b, "\n        and %d more", m.More)
	}
	if m.Link != "" {
		b.WriteString("\n        " + m.Link)
	}
	m.Text = b.String()
	return m
}

func plural(n int, suffix string) string {
	if n == 1 {
		return ""
	}
	return suffix
}

// Count formats a number with a space between thousands: 1 204.
func Count(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}
