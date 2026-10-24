//go:build integration

package scope_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
	"github.com/JoshuaMart/recon/internal/store/storetest"
)

type fakeSource struct{ entries []scope.Entry }

func (f *fakeSource) Scopes(context.Context) ([]scope.Entry, error) { return f.entries, nil }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Minute); return c.t }

func entry(platform, slug, value, typ string) scope.Entry {
	return scope.Entry{Platform: platform, Slug: slug, ProgramName: slug + " program", Value: value, Type: typ, IsInScope: true, Bounty: true}
}

func newSyncer(st *store.Store, src scope.Source) *scope.Syncer {
	c := &clock{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	return &scope.Syncer{
		Store: st, Source: src, MaxRemovedRatio: 0.5, Now: c.now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func programBySlug(t *testing.T, st *store.Store, slug string) sqlcgen.Program {
	t.Helper()
	rows, err := st.Q.ListPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Program.Slug != nil && *r.Program.Slug == slug {
			p, err := st.Q.GetProgram(context.Background(), r.Program.ID)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatalf("no program %q", slug)
	return sqlcgen.Program{}
}

func openPatterns(t *testing.T, st *store.Store, id uuid.UUID) map[string]bool {
	t.Helper()
	rules, err := st.Q.ListRules(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rules {
		if r.ValidTo == nil {
			out[r.Origin+" "+r.Kind+" "+r.Matcher+" "+r.Pattern] = true
		}
	}
	return out
}

func TestSyncReconciles(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	src := &fakeSource{entries: []scope.Entry{
		entry("hackerone", "acme", "*.acme.com", "web"),
		entry("hackerone", "acme", "api.acme.com", "web"),
		entry("hackerone", "acme", "com.acme.app", "mobile"),
		entry("yeswehack", "foo", "*.foo.io", "web"),
		entry("intigriti", "bar", "https://bar.dev/app/", "web"),
	}}
	s := newSyncer(st, src)

	sum, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Created != 3 || sum.RulesOpened != 4 || sum.Untracked != 1 {
		t.Fatalf("first pass = %+v", sum)
	}
	acme := programBySlug(t, st, "acme")
	if got := openPatterns(t, st, acme.ID); !got["sync include apex acme.com"] || !got["sync include fqdn api.acme.com"] || len(got) != 2 {
		t.Fatalf("acme rules = %v", got)
	}
	untracked, err := st.Q.ListUntracked(ctx, acme.ID)
	if err != nil || len(untracked) != 1 || untracked[0].Value != "com.acme.app" {
		t.Fatalf("untracked = %v, %v", untracked, err)
	}

	// The same answer again changes nothing.
	sum, err = s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Created != 0 || sum.RulesOpened != 0 || sum.RulesClosed != 0 || sum.Archived != 0 {
		t.Fatalf("idempotent pass = %+v", sum)
	}

	// A local exclusion survives a pass, and a removed scope closes its rule.
	if _, err := scope.OpenRule(ctx, st.Q, acme.ID, scope.Rule{Kind: scope.Exclude, Matcher: scope.FQDN, Pattern: "admin.acme.com"}, "local", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	src.entries = append(src.entries[:1:1], src.entries[2:]...) // drop api.acme.com
	sum, err = s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RulesClosed != 1 {
		t.Fatalf("pass after removal = %+v", sum)
	}
	got := openPatterns(t, st, acme.ID)
	if got["sync include fqdn api.acme.com"] || !got["local exclude fqdn admin.acme.com"] || !got["sync include apex acme.com"] {
		t.Fatalf("acme rules after removal = %v", got)
	}
	rules, _ := st.Q.ListRules(ctx, acme.ID)
	closed := 0
	for _, r := range rules {
		if r.ValidTo != nil {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("a closed rule must stay as history, found %d closed", closed)
	}
}

func TestSyncArchivesAndReactivates(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	all := []scope.Entry{
		entry("hackerone", "a", "*.a.com", "web"),
		entry("hackerone", "b", "*.b.com", "web"),
		entry("hackerone", "c", "*.c.com", "web"),
	}
	src := &fakeSource{entries: all}
	s := newSyncer(st, src)
	if _, err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// You archive b yourself.
	b := programBySlug(t, st, "b")
	user := "user"
	if err := st.Q.SetProgramState(ctx, sqlcgen.SetProgramStateParams{ID: b.ID, State: "archived", ArchivedBy: &user, Now: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// c leaves the platform: one of two active programs, at the 50 % bound.
	src.entries = all[:2]
	sum, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Archived != 1 {
		t.Fatalf("pass = %+v", sum)
	}
	c := programBySlug(t, st, "c")
	if c.State != "archived" || *c.ArchivedBy != "sync" {
		t.Fatalf("c = %s by %v", c.State, c.ArchivedBy)
	}
	if got := openPatterns(t, st, c.ID); !got["sync include apex c.com"] {
		t.Fatalf("an archived program keeps its rules, got %v", got)
	}

	// Everything comes back: c is reactivated, b stays as you left it.
	src.entries = all
	sum, err = s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Reactivated != 1 {
		t.Fatalf("pass = %+v", sum)
	}
	if c := programBySlug(t, st, "c"); c.State != "active" || c.ArchivedBy != nil {
		t.Fatalf("c = %s by %v", c.State, c.ArchivedBy)
	}
	if b := programBySlug(t, st, "b"); b.State != "archived" {
		t.Fatalf("b = %s, the sync must not undo your archive", b.State)
	}
}

func TestSyncRefusesASuspiciousAnswer(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	all := []scope.Entry{
		entry("hackerone", "a", "*.a.com", "web"),
		entry("hackerone", "b", "*.b.com", "web"),
		entry("hackerone", "c", "*.c.com", "web"),
	}
	src := &fakeSource{entries: all}
	s := newSyncer(st, src)
	if _, err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}

	for name, entries := range map[string][]scope.Entry{
		"empty answer":          nil,
		"two of three programs": all[:1],
	} {
		src.entries = entries
		if _, err := s.Run(ctx); !errors.Is(err, scope.ErrSuspicious) {
			t.Fatalf("%s: err = %v, want ErrSuspicious", name, err)
		}
		for _, slug := range []string{"a", "b", "c"} {
			if p := programBySlug(t, st, slug); p.State != "active" {
				t.Fatalf("%s: %s = %s, a refused pass must change nothing", name, slug, p.State)
			}
		}
	}
	state, err := st.Q.GetSyncState(ctx)
	if err != nil || state.LastError == nil {
		t.Fatalf("the refusal must be recorded, got %+v, %v", state, err)
	}
}
