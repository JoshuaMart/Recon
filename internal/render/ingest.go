// Package render dispatches due renders to the Fingerprinter, paced to what it
// sustains, and writes what comes back: the fingerprint observation, the
// technologies, the screenshot.
package render

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/inventory"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Settings are the render cadences and the screenshot policy.
type Settings struct {
	Periodic time.Duration // next render after a successful one
	Retry    time.Duration // next render after the target did not answer
	Quality  int           // JPEG quality of stored screenshots
	Keep     int           // distinct screenshots kept per asset
	Jitter   float64
}

// DefaultSettings are the documented defaults.
var DefaultSettings = Settings{Periodic: 21 * 24 * time.Hour, Retry: 6 * time.Hour, Quality: 70, Keep: 3, Jitter: 0.1}

// Target is one due render.
type Target struct {
	AssetID   uuid.UUID
	ProgramID uuid.UUID
	Kind      string
	Key       string
	Host      string
	Port      int
	Scheme    string
}

// URL is what the browser opens: the service root, or a declared URL as
// written.
func (t Target) URL() string {
	if t.Kind == inventory.KindURL {
		return t.Key
	}
	scheme := t.Scheme
	if scheme == "" {
		scheme = "https"
		if t.Port == 80 {
			scheme = "http"
		}
	}
	host := t.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (scheme == "https" && t.Port != 443) || (scheme == "http" && t.Port != 80) {
		host += ":" + strconv.Itoa(t.Port)
	}
	return scheme + "://" + host + "/"
}

// Writer writes render outcomes.
type Writer struct {
	Store    *store.Store
	Storage  fingerprint.Storage
	Settings Settings
	Now      func() time.Time
	Rand     func() float64
}

func (w *Writer) now() time.Time { return w.Now().UTC().Truncate(time.Microsecond) }

func (w *Writer) after(d time.Duration) time.Time {
	r := w.Rand
	if r == nil {
		r = rand.Float64 //nolint:gosec // jitter
	}
	return w.now().Add(time.Duration(float64(d) * (1 + w.Settings.Jitter*(2*r()-1))))
}

// retryAfter is the wait after this many target failures in a row: Retry,
// doubled each time, never past Periodic. A dead service is not retried
// every few hours forever; verification still sees it come back.
func (w *Writer) retryAfter(failures int) time.Duration {
	d := w.Settings.Retry
	for i := 1; i < failures && d < w.Settings.Periodic; i++ {
		d *= 2
	}
	return min(d, w.Settings.Periodic)
}

// Outcome is what writing a render did.
type Outcome struct {
	Changed        bool
	TechChanges    int
	ScreenshotKept bool
	ScreenshotSize int
	// ScreenshotError is why a screenshot the render carried was not usable.
	ScreenshotError error
}

// Result writes a successful render.
func (w *Writer) Result(ctx context.Context, t Target, r *fingerprint.Result) (Outcome, error) {
	var out Outcome
	now := w.now()
	// The screenshot goes to storage first: a file without its row is an
	// orphan a later render overwrites, a row without its file is a broken page.
	var shot *fingerprint.Shot
	if r.Screenshot != nil && *r.Screenshot != "" {
		s, err := fingerprint.Prepare(*r.Screenshot, w.Settings.Quality)
		if err != nil {
			out.ScreenshotError = err
		} else {
			shot = &s
		}
	}

	tx, err := w.Store.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.Store.Q.WithTx(tx)
	wr := &inventory.Writer{Q: q, ProgramID: t.ProgramID, Now: now, ProgramActive: true}

	cur, err := q.GetAssetCurrent(ctx, t.AssetID)
	if err != nil {
		return out, err
	}
	data := fingerprint.Normalize(r)
	obs, err := wr.Observe(ctx, t.AssetID, "fingerprint", "ok", data, r.Version)
	if err != nil {
		return out, err
	}
	// The first successful render is a first sight, not a change: its
	// technologies are stored without a change each.
	n, err := w.technologies(ctx, q, wr, t.AssetID, r.Technologies, cur.LastRenderAt != nil)
	if err != nil {
		return out, err
	}
	out.TechChanges = n
	if obs.Inserted && obs.Before != nil {
		out.Changed = true
		if other, err := otherThanTech(obs.Before, data); err != nil {
			return out, err
		} else if other {
			if err := wr.Change(ctx, t.AssetID, "render_changed", "fingerprint", obs.Before, data); err != nil {
				return out, err
			}
		}
	}
	if err := q.SetRenderCurrent(ctx, current(t.AssetID, data, r.Technologies)); err != nil {
		return out, err
	}
	if _, _, err := wr.Settle(ctx, inventory.Settle{ID: t.AssetID, Kind: t.Kind, Outcomes: map[string]string{"fingerprint": "ok"}, Verify: true}); err != nil {
		return out, err
	}
	if err := q.ScheduleRender(ctx, sqlcgen.ScheduleRenderParams{AssetID: t.AssetID, Next: w.after(w.Settings.Periodic), RenderedAt: &now}); err != nil {
		return out, err
	}
	if shot != nil {
		kept, err := w.screenshot(ctx, q, t.AssetID, *shot, now)
		if err != nil {
			return out, err
		}
		out.ScreenshotKept, out.ScreenshotSize = kept, len(shot.Image)
	}
	return out, tx.Commit(ctx)
}

// Unreachable writes a render where the target did not answer the browser: a
// measurement, which feeds unobservable and never a death.
func (w *Writer) Unreachable(ctx context.Context, t Target, reason string) error {
	now := w.now()
	tx, err := w.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.Store.Q.WithTx(tx)
	wr := &inventory.Writer{Q: q, ProgramID: t.ProgramID, Now: now, ProgramActive: true}
	if _, err := wr.Observe(ctx, t.AssetID, "fingerprint", "error", map[string]string{"browser": reason}, ""); err != nil {
		return err
	}
	if _, _, err := wr.Settle(ctx, inventory.Settle{ID: t.AssetID, Kind: t.Kind, Outcomes: map[string]string{"fingerprint": "error"}, Verify: true}); err != nil {
		return err
	}
	failures, err := q.RecordRenderFailure(ctx, t.AssetID)
	if err != nil {
		return err
	}
	if err := q.SetNextRender(ctx, sqlcgen.SetNextRenderParams{AssetID: t.AssetID, Next: w.after(w.retryAfter(int(failures)))}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// technologies replaces the asset's set and writes one change per difference.
func (w *Writer) technologies(ctx context.Context, q *sqlcgen.Queries, wr *inventory.Writer, id uuid.UUID, techs []fingerprint.Technology, record bool) (int, error) {
	old, err := q.ListTech(ctx, id)
	if err != nil {
		return 0, err
	}
	before := map[string]sqlcgen.AssetTech{}
	for _, t := range old {
		before[t.Name] = t
	}
	changes := 0
	seen := map[string]bool{}
	for _, t := range techs {
		name := strings.TrimSpace(t.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		version := strings.TrimSpace(t.Version)
		var proof []byte
		if len(t.Proof) > 0 && string(t.Proof) != "null" {
			proof = t.Proof
		}
		if err := q.PutTech(ctx, sqlcgen.PutTechParams{
			AssetID: id, Name: name, Category: t.Category, VersionRaw: version,
			Version: fingerprint.ParseVersion(version), Cpe: optStr(t.CPE), Proof: proof,
		}); err != nil {
			return 0, err
		}
		prev, had := before[name]
		switch {
		case !record:
		case !had:
			changes++
			if err := wr.Change(ctx, id, "tech_added", "fingerprint", nil, map[string]string{"name": name, "version": version}); err != nil {
				return 0, err
			}
		case prev.VersionRaw != version:
			changes++
			if err := wr.Change(ctx, id, "tech_version_changed", "fingerprint",
				map[string]string{"name": name, "version": prev.VersionRaw},
				map[string]any{"name": name, "version": version, "precision": fingerprint.Precision(prev.VersionRaw, version)}); err != nil {
				return 0, err
			}
		}
	}
	for name, prev := range before {
		if seen[name] {
			continue
		}
		if err := q.DeleteTech(ctx, sqlcgen.DeleteTechParams{AssetID: id, Name: name}); err != nil {
			return 0, err
		}
		if !record {
			continue
		}
		changes++
		if err := wr.Change(ctx, id, "tech_removed", "fingerprint", map[string]string{"name": name, "version": prev.VersionRaw}, nil); err != nil {
			return 0, err
		}
	}
	return changes, nil
}

// otherThanTech reports whether two fingerprint observations differ in
// anything but their technologies, which have their own changes.
func otherThanTech(before json.RawMessage, after fingerprint.Data) (bool, error) {
	var b fingerprint.Data
	if err := json.Unmarshal(before, &b); err != nil {
		return false, err
	}
	b.Technologies, after.Technologies = nil, nil
	bj, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	aj, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	return string(bj) != string(aj), nil
}

// screenshot keeps a render's screenshot unless it looks like the last one,
// and drops the rows beyond the cap. Files are never deleted here: keys are
// content-addressed and shared, so whether a file is still named is only safe
// to decide outside every render's transaction, by CollectGarbage.
func (w *Writer) screenshot(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID, s fingerprint.Shot, now time.Time) (bool, error) {
	last, err := q.LastScreenshot(ctx, id)
	switch {
	case err == nil && fingerprint.Distance(last.Phash, s.PHash) <= fingerprint.SameImage:
		return false, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	if err := w.Storage.Put(ctx, s.Key, s.Image); err != nil {
		return false, err
	}
	if err := w.Storage.Put(ctx, s.ThumbKey, s.Thumb); err != nil {
		return false, err
	}
	if _, err := q.InsertScreenshot(ctx, sqlcgen.InsertScreenshotParams{
		AssetID: id, Key: s.Key, ThumbKey: s.ThumbKey, Phash: s.PHash,
		Bytes: int32(len(s.Image)), Width: int32(s.Width), Height: int32(s.Height), Now: now, //nolint:gosec // image sizes
	}); err != nil {
		return false, err
	}
	old, err := q.ScreenshotsBeyond(ctx, sqlcgen.ScreenshotsBeyondParams{AssetID: id, Keep: int32(w.Settings.Keep)}) //nolint:gosec // a small cap
	if err != nil {
		return false, err
	}
	for _, o := range old {
		if err := q.DeleteScreenshot(ctx, o.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// gcGrace is how old a file must be before garbage collection may delete it:
// a render writes its files before its transaction commits.
const gcGrace = time.Hour

// CollectGarbage deletes the stored files no screenshot row names and older
// than the grace period: those of pruned rows and those of a render whose
// transaction rolled back. It returns how many it deleted.
func CollectGarbage(ctx context.Context, q *sqlcgen.Queries, storage fingerprint.Storage, now time.Time) (int, error) {
	var batch []string
	deleted := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		used, err := q.ScreenshotKeysInUse(ctx, batch)
		if err != nil {
			return err
		}
		for _, k := range batch {
			if slices.Contains(used, k) {
				continue
			}
			if err := storage.Delete(ctx, k); err != nil {
				return err
			}
			deleted++
		}
		batch = batch[:0]
		return nil
	}
	err := storage.List(ctx, func(key string, written time.Time) error {
		if now.Sub(written) < gcGrace {
			return nil
		}
		batch = append(batch, key)
		if len(batch) == 500 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return deleted, err
	}
	return deleted, flush()
}

// current projects a render onto asset_current, for search.
func current(id uuid.UUID, d fingerprint.Data, techs []fingerprint.Technology) sqlcgen.SetRenderCurrentParams {
	var redirectHosts, headerNames, scriptHashes []string
	for _, h := range d.Chain {
		if u, err := url.Parse(h.URL); err == nil && u.Hostname() != "" {
			redirectHosts = append(redirectHosts, strings.ToLower(u.Hostname()))
		}
		for name := range h.Headers {
			headerNames = append(headerNames, name)
		}
	}
	for _, s := range d.Scripts {
		if s.Hash != "" {
			scriptHashes = append(scriptHashes, s.Hash)
		}
	}
	var waf *string
	for _, t := range techs {
		if strings.EqualFold(t.Category, "WAF") {
			name := strings.ToLower(t.Name)
			waf = &name
			break
		}
	}
	robots, llms, sitemap := d.Robots, d.LLMsTXT, d.Sitemap != ""
	// The last hop's headers, for header.<name> filters: the page that answered.
	var headers []byte
	if len(d.Chain) > 0 {
		headers, _ = json.Marshal(d.Chain[len(d.Chain)-1].Headers)
	}
	return sqlcgen.SetRenderCurrentParams{
		AssetID: id, RedirectHosts: set(redirectHosts), HeaderNames: set(headerNames), CookieNames: d.CookieNames,
		ScriptHashes: set(scriptHashes), FaviconHash: optStr(d.FaviconHash), ExternalHosts: d.ExternalHosts,
		WebSockets: d.WebSockets, HasRobots: &robots, HasSitemap: &sitemap, HasLlmsTxt: &llms, Waf: waf,
		Headers: headers,
	}
}

func set(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
