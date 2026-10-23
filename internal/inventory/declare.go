package inventory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/canon"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Declared is what declaring one entry did.
type Declared struct {
	Entry   string `json:"entry"`
	Kind    string `json:"kind,omitempty"`
	Key     string `json:"key,omitempty"`
	Created bool   `json:"created"`
	Scope   string `json:"scope,omitempty"`
	Error   string `json:"error,omitempty"`
	// HostKey is the host the entry sits on, when it is a name.
	HostKey string `json:"-"`
}

// Declare puts a host or a URL in the inventory without observing it: a host
// becomes an fqdn or ip asset, a URL its host, its service and itself. Every
// declared asset starts as a candidate, since nothing has answered yet.
func (w *Writer) Declare(ctx context.Context, entry, source string) (Declared, error) {
	out := Declared{Entry: entry}
	e := strings.TrimSpace(entry)
	if strings.Contains(e, "://") {
		return w.declareURL(ctx, out, e, source)
	}
	host, err := canonicalHost(e)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	kind, key := HostAsset(host)
	wr, err := w.Upsert(ctx, Asset{
		ProgramID: w.ProgramID, Kind: kind, Key: key, Host: key, Source: source,
		Path: []map[string]any{{"step": "declared", "source": source}}, Lifecycle: "candidate",
	})
	if err != nil {
		return out, err
	}
	out.Kind, out.Key, out.Created, out.Scope = kind, key, wr.New, wr.Scope
	if kind == KindFQDN {
		out.HostKey = key
	}
	return out, nil
}

func (w *Writer) declareURL(ctx context.Context, out Declared, e, source string) (Declared, error) {
	u, err := canon.URL(e)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	scheme, rest, _ := strings.Cut(u, "://")
	authority, _, _ := strings.Cut(rest, "/")
	host, port := authority, 443
	if scheme == "http" {
		port = 80
	}
	if i := strings.LastIndex(authority, ":"); i > strings.LastIndex(authority, "]") {
		p, err := strconv.Atoi(authority[i+1:])
		if err != nil {
			out.Error = err.Error()
			return out, nil
		}
		host, port = authority[:i], p
	}
	host = strings.Trim(host, "[]")
	path := []map[string]any{{"step": "declared", "source": source, "url": u}}

	kind, key := HostAsset(host)
	h, err := w.Upsert(ctx, Asset{ProgramID: w.ProgramID, Kind: kind, Key: key, Host: key, Source: source, Path: path, Lifecycle: "candidate"})
	if err != nil {
		return out, err
	}
	svc, err := w.Upsert(ctx, Asset{
		ProgramID: w.ProgramID, Kind: KindService, Key: ServiceKey(key, port), Host: key, Port: port, Scheme: scheme,
		Parent: &h.ID, Source: source, Path: path, Lifecycle: "candidate",
	})
	if err != nil {
		return out, err
	}
	wr, err := w.Upsert(ctx, Asset{
		ProgramID: w.ProgramID, Kind: KindURL, Key: u, Host: key, Port: port, Scheme: scheme,
		Parent: &svc.ID, Source: source, Path: path, Lifecycle: "candidate",
	})
	if err != nil {
		return out, err
	}
	out.Kind, out.Key, out.Created, out.Scope = KindURL, u, wr.New, wr.Scope
	if kind == KindFQDN {
		out.HostKey = key
	}
	return out, nil
}

func canonicalHost(s string) (string, error) {
	if p, err := canon.Prefix(s); err == nil && p.IsSingleIP() {
		return p.Addr().String(), nil
	}
	return canon.FQDN(s)
}

// Rules loads the rules in force for a program as a classifier.
func Rules(ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID) (*scope.Classifier, []scope.Rule, error) {
	rows, err := q.ListOpenRules(ctx, programID)
	if err != nil {
		return nil, nil, err
	}
	rules := make([]scope.Rule, 0, len(rows))
	for _, r := range rows {
		rules = append(rules, scope.Rule{Kind: r.Kind, Matcher: r.Matcher, Pattern: r.Pattern})
	}
	return scope.NewClassifier(rules), rules, nil
}

// Refreshed is what a refresh changed.
type Refreshed struct {
	Declared     int `json:"declared"`
	Reclassified int `json:"reclassified"`
	AutoScans    int `json:"auto_scans"`
}

// Refresh brings a program's inventory in line with its rules, in the
// transaction that wrote them: what an include rule names is declared, every
// asset is classified again, and a wildcard created now is queued for recon
// when auto-scan says so.
func Refresh(ctx context.Context, q *sqlcgen.Queries, p sqlcgen.Program, defaultAutoScan string, now time.Time) (Refreshed, error) {
	c, rules, err := Rules(ctx, q, p.ID)
	if err != nil {
		return Refreshed{}, err
	}
	w := &Writer{Q: q, ProgramID: p.ID, Classifier: c, Now: now}
	source := "manual"
	if p.Origin == "scopesextractor" {
		source = "scopesextractor"
	}
	var res Refreshed
	for _, r := range rules {
		if r.Kind != scope.Include || (r.Matcher != scope.FQDN && r.Matcher != scope.URLPrefix) {
			continue
		}
		d, err := w.Declare(ctx, r.Pattern, source)
		if err != nil {
			return res, fmt.Errorf("declare %s: %w", r, err)
		}
		if d.Created {
			res.Declared++
		}
	}

	assets, err := q.ListProgramAssets(ctx, p.ID)
	if err != nil {
		return res, err
	}
	for _, a := range assets {
		t := scope.Target{Kind: a.Kind, Key: a.Key, Host: a.Host, Port: int(deref(a.Port)), Scheme: derefS(a.Scheme)}
		status := c.Classify(t)
		if status == a.ScopeStatus {
			continue
		}
		if err := q.SetAssetScope(ctx, sqlcgen.SetAssetScopeParams{AssetID: a.ID, ScopeStatus: status}); err != nil {
			return res, err
		}
		if err := w.change(ctx, a.ID, "scope_changed", "", map[string]any{"scope": a.ScopeStatus}, map[string]any{"scope": status}); err != nil {
			return res, err
		}
		res.Reclassified++
	}

	if err := SyncSchedule(ctx, q, p.ID, now); err != nil {
		return res, err
	}

	autoScan := defaultAutoScan
	if p.AutoScan != nil {
		autoScan = *p.AutoScan
	}
	if autoScan == "on_new" && p.State == "active" {
		fresh, err := q.NewWildcards(ctx, sqlcgen.NewWildcardsParams{ProgramID: p.ID, Now: now})
		if err != nil {
			return res, err
		}
		for _, id := range fresh {
			if _, err := q.EnqueueRun(ctx, sqlcgen.EnqueueRunParams{
				ProgramID: p.ID, Kind: "recon", Stage: "full", WildcardID: &id, Trigger: "auto_scan", Now: now,
			}); err != nil && !isNoRows(err) {
				return res, err
			}
			res.AutoScans++
		}
	}
	return res, nil
}

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func derefS(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// RefreshAll refreshes every program, each in its own transaction. Run at
// startup, it catches up on rules written while the inventory could not follow,
// such as rules that predate a migration.
func RefreshAll(ctx context.Context, pool *pgxpool.Pool, q *sqlcgen.Queries, now time.Time) (Refreshed, error) {
	programs, err := q.ListPrograms(ctx)
	if err != nil {
		return Refreshed{}, err
	}
	var total Refreshed
	for _, row := range programs {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return total, err
		}
		tq := q.WithTx(tx)
		p, err := tq.GetProgram(ctx, row.Program.ID)
		if err == nil {
			var r Refreshed
			// Not a new wildcard: auto-scan reacts to creation, not to a restart.
			r, err = Refresh(ctx, tq, p, "off", now)
			total.Declared += r.Declared
			total.Reclassified += r.Reclassified
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return total, fmt.Errorf("program %s: %w", row.Program.ID, err)
		}
	}
	return total, nil
}
