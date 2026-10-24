//go:build integration

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/search"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
	"github.com/JoshuaMart/recon/internal/store/storetest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)} }
func notifications(t *testing.T, st *store.Store) []sqlcgen.Notification {
	t.Helper()
	rows, err := st.Q.RecentNotifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func program(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO program (origin, name, created_at, updated_at) VALUES ('manual', 'Acme', now(), now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func host(t *testing.T, st *store.Store, pid uuid.UUID, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := st.Pool.QueryRow(ctx, `INSERT INTO asset (program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1, 'fqdn', $2, $2, 'manual', 'in_scope', now(), now()) RETURNING id`, pid, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO asset_current (asset_id, program_id, kind, key, host, scope_status, lifecycle, first_seen, last_seen, lifecycle_since)
		VALUES ($1, $2, 'fqdn', $3, $3, 'in_scope', 'active', now(), now(), now())`, id, pid, name); err != nil {
		t.Fatal(err)
	}
	return id
}

func enqueue(t *testing.T, st *store.Store, e Event, now time.Time) {
	t.Helper()
	if err := Enqueue(context.Background(), st.Q, e, now); err != nil {
		t.Fatal(err)
	}
}

func TestEventsOfAGroupJoinItsOpenWindow(t *testing.T) {
	st := storetest.New(t)
	c := newClock()
	e := Event{Kind: KindSavedSearch, Group: "search:1", Title: "Old Jenkins", Window: SavedSearchWindow}

	e.Items, e.Count = []string{"a.acme.com"}, 1
	enqueue(t, st, e, c.now())
	c.add(10 * time.Minute)
	e.Items, e.Count = []string{"b.acme.com", "c.acme.com"}, 2
	enqueue(t, st, e, c.now())
	if got := notifications(t, st); len(got) != 1 || got[0].Total != 3 || len(got[0].Items) != 3 {
		t.Fatalf("one window, three items: got %+v", got)
	}
	// The window is trailing from its first event: past it, a new one opens.
	c.add(6 * time.Minute)
	e.Items, e.Count = []string{"d.acme.com"}, 1
	enqueue(t, st, e, c.now())
	if got := notifications(t, st); len(got) != 2 {
		t.Fatalf("after the window, a second notification: got %d", len(got))
	}
}

func TestItemsAreCappedButCounted(t *testing.T) {
	st := storetest.New(t)
	c := newClock()
	items := make([]string, 25)
	for i := range items {
		items[i] = "h" + string(rune('a'+i))
	}
	enqueue(t, st, Event{Kind: KindSavedSearch, Group: "search:1", Title: "All", Items: items, Count: 25, Window: time.Minute}, c.now())
	got := notifications(t, st)[0]
	if len(got.Items) != MaxItems || got.Total != 25 {
		t.Fatalf("items %d total %d, want %d and 25", len(got.Items), got.Total, MaxItems)
	}
	if m := Compose(got); m.More != 15 || !strings.Contains(m.Text, "and 15 more") || !strings.Contains(m.Title, "25 new matches") {
		t.Fatalf("message = %+v", m)
	}
}

func TestAlertNotifiesOnlyWhatEnters(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	c := newClock()
	pid := program(t, st)
	host(t, st, pid, "a.acme.com")
	s, err := st.Q.CreateSavedSearch(ctx, sqlcgen.CreateSavedSearchParams{Name: "Hosts", Query: "kind:fqdn -triage:ignored", View: "services", Alert: true, Now: c.now()})
	if err != nil {
		t.Fatal(err)
	}
	ev := &Evaluator{Pool: st.Pool, Q: st.Q, Log: quiet, Now: c.now, PublicURL: "http://recon.test"}
	pass := func() {
		t.Helper()
		if err := ev.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}

	pass()
	if got := notifications(t, st); len(got) != 0 {
		t.Fatalf("switching an alert on notified the inventory it already matches: %+v", got)
	}

	b := host(t, st, pid, "b.acme.com")
	c.add(time.Minute)
	pass()
	got := notifications(t, st)
	if len(got) != 1 || got[0].Total != 1 || got[0].Items[0] != "b.acme.com" {
		t.Fatalf("b entered: got %+v", got)
	}
	c.add(time.Minute)
	pass()
	if got := notifications(t, st); got[0].Total != 1 {
		t.Fatalf("a match that stays notified again: total %d", got[0].Total)
	}

	// Leaving drops the match, so coming back notifies again.
	if _, err := st.Pool.Exec(ctx, `INSERT INTO triage (asset_id, state) VALUES ($1, 'ignored')`, b); err != nil {
		t.Fatal(err)
	}
	pass()
	if _, err := st.Pool.Exec(ctx, `UPDATE triage SET state = 'seen' WHERE asset_id = $1`, b); err != nil {
		t.Fatal(err)
	}
	pass()
	if got := notifications(t, st); got[0].Total != 2 {
		t.Fatalf("b left and came back: total %d, want 2", got[0].Total)
	}
	if got := notifications(t, st)[0]; got.Link == nil || !strings.HasPrefix(*got.Link, "http://recon.test/?q=") {
		t.Fatalf("link = %v", got.Link)
	}
	_ = s
}

func TestWatcherAnnouncesABreakOnceThenTheRecovery(t *testing.T) {
	st := storetest.New(t)
	c := newClock()
	ok := false
	w := &Watcher{Pool: st.Pool, Q: st.Q, Log: quiet, Now: c.now, BrokenAfter: 5 * time.Minute,
		Checks: []Check{{Component: "FastRecon", Probe: func(context.Context) (bool, string) { return ok, "unreachable" }}}}
	pass := func() {
		t.Helper()
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	kinds := func() []string {
		var out []string
		for _, n := range notifications(t, st) {
			out = append(out, n.Kind)
		}
		return out
	}

	pass()
	c.add(4 * time.Minute)
	pass()
	if got := kinds(); len(got) != 0 {
		t.Fatalf("announced before broken_after: %v", got)
	}
	c.add(2 * time.Minute)
	pass()
	c.add(time.Minute)
	pass()
	if got := kinds(); len(got) != 1 || got[0] != KindBroken {
		t.Fatalf("want one broken, got %v", got)
	}
	ok = true
	pass()
	pass()
	if got := kinds(); len(got) != 2 || got[0] != KindRecovered {
		t.Fatalf("want one recovered after it, got %v", got)
	}

	// A blip shorter than broken_after says nothing.
	ok = false
	pass()
	ok = true
	pass()
	if got := kinds(); len(got) != 2 {
		t.Fatalf("a short blip notified: %v", got)
	}
}

func TestSenderRetriesThenDelivers(t *testing.T) {
	st := storetest.New(t)
	c := newClock()
	var calls atomic.Int32
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	enqueue(t, st, Event{Kind: KindReconFinished, Group: "run:1", Title: "*.acme.com finished: 3 hosts, 1 new, 0 new services @everyone", Link: "http://recon.test/changes?run=1"}, c.now())
	s := &Sender{Q: st.Q, Channels: []Channel{Discord{URL: srv.URL + "/hook/secret-token"}}, Log: quiet, Now: c.now}
	if err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := notifications(t, st)[0]
	if n.SentAt != nil || n.Attempts != 1 || n.LastError == nil {
		t.Fatalf("a failed send is retried later: %+v", n)
	}
	if strings.Contains(*n.LastError, "secret-token") {
		t.Fatalf("the webhook URL reached the stored error: %s", *n.LastError)
	}
	if err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("retried before its backoff")
	}
	c.add(time.Minute)
	if err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	n = notifications(t, st)[0]
	if n.SentAt == nil || *n.Outcome != "sent" {
		t.Fatalf("not delivered after the backoff: %+v", n)
	}
	am, _ := body["allowed_mentions"].(map[string]any)
	if parse, _ := am["parse"].([]any); am == nil || len(parse) != 0 {
		t.Fatalf("mentions are not disabled: %v", body)
	}
	if content, _ := body["content"].(string); !strings.HasPrefix(content, "```") || !strings.HasSuffix(content, "http://recon.test/changes?run=1") {
		t.Fatalf("content = %q", content)
	}
}

func TestNoChannelKeepsTheNotification(t *testing.T) {
	st := storetest.New(t)
	c := newClock()
	enqueue(t, st, Event{Kind: KindBroken, Group: "health:x", Title: "x is failing"}, c.now())
	if err := (&Sender{Q: st.Q, Log: quiet, Now: c.now}).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := notifications(t, st)[0]; n.SentAt == nil || *n.Outcome != "no_channel" {
		t.Fatalf("got %+v", n)
	}
}

func TestUnreachableChannelKeepsItsTokenOutOfTheError(t *testing.T) {
	st := storetest.New(t)
	c := newClock()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/api/webhooks/1/secret-token"
	srv.Close() // refused from now on
	enqueue(t, st, Event{Kind: KindBroken, Group: "health:x", Title: "x is failing"}, c.now())
	if err := (&Sender{Q: st.Q, Channels: []Channel{Discord{URL: url}}, Log: quiet, Now: c.now}).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := notifications(t, st)[0]
	if n.LastError == nil || strings.Contains(*n.LastError, "secret-token") {
		t.Fatalf("stored error = %v", n.LastError)
	}
}

// Past the cap, a sample would differ from pass to pass and read as entries:
// the search is left as it was instead.
func TestSearchPastTheCapIsNotEvaluated(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	c := newClock()
	pid := program(t, st)
	host(t, st, pid, "a.acme.com")
	if _, err := st.Q.CreateSavedSearch(ctx, sqlcgen.CreateSavedSearchParams{Name: "All", Query: "kind:fqdn", View: "services", Alert: true, Now: c.now()}); err != nil {
		t.Fatal(err)
	}
	ev := &Evaluator{Pool: st.Pool, Q: st.Q, Log: quiet, Now: c.now}
	if err := ev.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	host(t, st, pid, "b.acme.com")
	cap := search.MaxMatches
	search.MaxMatches = 1
	defer func() { search.MaxMatches = cap }()
	for range 3 {
		if err := ev.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := notifications(t, st); len(got) != 0 {
		t.Fatalf("a search past the cap notified: %+v", got)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM saved_search_match`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("matches = %d %v, want the one recorded before the cap", n, err)
	}
}

type recorder struct {
	name  string
	fails int
	got   int
}

func (r *recorder) Name() string { return r.name }
func (r *recorder) Send(context.Context, Message) error {
	r.got++
	if r.fails > 0 {
		r.fails--
		return errBoom
	}
	return nil
}

var errBoom = errors.New("boom")

// One channel down must not make the others repeat themselves.
func TestRetryGoesOnlyToTheChannelsThatFailed(t *testing.T) {
	st := storetest.New(t)
	c := newClock()
	ok, down := &recorder{name: "discord"}, &recorder{name: "webhook", fails: 2}
	enqueue(t, st, Event{Kind: KindBroken, Group: "health:x", Title: "x is failing"}, c.now())
	s := &Sender{Q: st.Q, Channels: []Channel{ok, down}, Log: quiet, Now: c.now}
	for range 3 {
		if err := s.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.add(time.Hour)
	}
	if ok.got != 1 || down.got != 3 {
		t.Fatalf("discord got %d, webhook %d: want 1 and 3", ok.got, down.got)
	}
	if n := notifications(t, st)[0]; n.Outcome == nil || *n.Outcome != "sent" {
		t.Fatalf("got %+v", n)
	}
}
