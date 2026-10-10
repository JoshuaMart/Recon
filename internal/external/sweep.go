// Package external answers the one question the inventory cannot: is someone else's domain
// still registered. The dangerous half of external_host_dead is invisible to the cross
// reference: an own host dies visibly, but a third party whose domain expired is
// re-registrable by anyone (the classic supply chain case).
//
// Its permission is bounded, which makes it defensible: it resolves the name and its apex
// and never opens a TCP connection, sends an HTTP request or renders. A public resolver
// query reaches neither the domain nor its infrastructure, so it costs no programme budget.
package external

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/publicsuffix"

	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Resolver answers whether an apex is still registered. An interface so no test
// depends on someone's expired domain staying expired.
type Resolver interface {
	// Registered is false only on an authoritative "no such domain". A timeout,
	// refusal or broken resolver are not, and treating them as expiry would raise a
	// critical alert on every asset of every tenant at the first bad minute.
	Registered(ctx context.Context, apex string) (bool, error)
}

// DNS resolves through the host's resolver.
type DNS struct {
	resolver *net.Resolver
	timeout  time.Duration
}

// NewDNS builds one.
func NewDNS(timeout time.Duration) *DNS {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &DNS{resolver: net.DefaultResolver, timeout: timeout}
}

// Registered asks for the apex's name servers. NS, not A: a parked registered
// domain with no address still has name servers, an expired one answers NXDOMAIN
// to everything, and asking for an address would call a parked domain
// re-registrable.
func (d *DNS) Registered(ctx context.Context, apex string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	_, err := d.resolver.LookupNS(ctx, apex)
	if err == nil {
		return true, nil
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false, nil
	}
	return true, err
}

// Sweep is the loop.
type Sweep struct {
	pool     *pgxpool.Pool
	resolver Resolver
	interval time.Duration
	batch    int
	now      func() time.Time
	log      *slog.Logger
}

// New builds it. On the system pool: it serves every tenant in one tick, and
// third-party hosts are deduplicated across them (a public CDN is one lookup,
// not one per tenant).
func New(pool *pgxpool.Pool, resolver Resolver, interval time.Duration, batch int, log *slog.Logger) *Sweep {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	if batch <= 0 {
		batch = 5000
	}
	return &Sweep{
		pool: pool, resolver: resolver, interval: interval,
		batch: batch, now: time.Now, log: log,
	}
}

// Run ticks until the context ends.
func (s *Sweep) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Once(ctx); err != nil {
				s.log.ErrorContext(ctx, "external host sweep failed", "error", err)
			}
		}
	}
}

// reference is one asset pointing at one third party host.
type reference struct {
	asset   uuid.UUID
	org     uuid.UUID
	program uuid.UUID
	key     string
	host    string
	// recorded is what the last tick concluded about this asset's hosts. A failed
	// lookup must leave those alone, so it travels with the row.
	recorded []string
}

// Once resolves every distinct apex once and writes what it found. It walks the
// whole set, not one capped page: a fixed cap in a fixed order would sweep the
// same lowest identifiers every tick and leave the rest permanently unseen.
func (s *Sweep) Once(ctx context.Context) error {
	queries := sqlcgen.New(s.pool)

	var references []reference
	apexes := map[string]bool{}
	after := uuid.Nil
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rows, err := queries.ThirdPartyHosts(ctx, sqlcgen.ThirdPartyHostsParams{
			After: pgUUID(after),
			Cap:   int32(s.batch), //nolint:gosec // bounded by configuration
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			after = uuid.UUID(row.AssetID.Bytes)
			host, _ := row.Host.(string)
			apex := Apex(host)
			if apex == "" {
				continue
			}
			references = append(references, reference{
				asset:    after,
				org:      uuid.UUID(row.OrgID.Bytes),
				program:  uuid.UUID(row.ProgramID.Bytes),
				key:      row.Key,
				host:     host,
				recorded: decode(row.Recorded),
			})
			apexes[apex] = false
		}
		// A page ends on an asset boundary, so a short page ends the walk. The cursor is
		// the asset, not the pair, keeping every host of one asset in one page and one
		// verdict.
		if len(rows) < s.batch {
			break
		}
	}
	if len(references) == 0 {
		return nil
	}

	// One lookup per distinct apex (dozens per deployment), not per reference. The
	// host itself is never resolved: a dead name under a still-registered domain is a
	// dangling subdomain at someone else's, not re-registrable and not this finding.
	gone := map[string]bool{}
	unknown := map[string]bool{}
	for apex := range apexes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		registered, err := s.resolver.Registered(ctx, apex)
		if err != nil {
			// Not a verdict, so not an answer: a bad resolver minute is neither an expiry nor
			// a return. Whatever the last tick concluded stands.
			s.log.WarnContext(ctx, "could not resolve a third party apex", "apex", apex, "error", err)
			unknown[apex] = true
			continue
		}
		gone[apex] = !registered
	}

	// Grouped per asset, as the verdict is written onto the asset as one list;
	// per reference, the last host would erase the others.
	dead := map[uuid.UUID][]string{}
	owner := map[uuid.UUID]reference{}
	for _, ref := range references {
		owner[ref.asset] = ref
		apex := Apex(ref.host)
		switch {
		case gone[apex]:
			dead[ref.asset] = append(dead[ref.asset], ref.host)
		case unknown[apex] && ref.was(ref.host):
			// Carried forward untouched. Dropping it and re-adding it next tick would read as
			// a new finding, so one timeout would re-send every critical alert here.
			dead[ref.asset] = append(dead[ref.asset], ref.host)
		}
	}

	found := 0
	written := 0
	for asset, ref := range owner {
		hosts := dead[asset]
		// A list repeating what it said is not written. In steady state that is every
		// asset with an external host: thousands of transactions and dead row versions
		// per tick to store what is there.
		if same(hosts, ref.recorded) {
			continue
		}
		emitted, err := s.record(ctx, ref, hosts)
		if err != nil {
			return err
		}
		written++
		found += emitted
	}
	if written > 0 {
		s.log.InfoContext(ctx, "third party hosts re-assessed",
			"references", len(references), "apexes", len(apexes),
			"unresolved", len(unknown), "assets", written, "events", found)
	}
	return nil
}

// was reports whether this asset already carried the host as dead.
func (r reference) was(host string) bool {
	for _, recorded := range r.recorded {
		if recorded == host {
			return true
		}
	}
	return false
}

// same compares two lists built in the walk order of one asset's external hosts,
// so they compare element by element.
func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// record writes the verdict and tells only what changed.
func (s *Sweep) record(ctx context.Context, ref reference, hosts []string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	before, err := q.MarkDeadExternalHosts(ctx, sqlcgen.MarkDeadExternalHostsParams{
		AssetID: pgUUID(ref.asset),
		Dead:    hosts,
	})
	if err != nil {
		return 0, err
	}

	known := map[string]bool{}
	for _, host := range decode(before) {
		known[host] = true
	}

	events := make([]notify.Event, 0, len(hosts))
	for _, host := range hosts {
		if known[host] {
			continue
		}
		asset := ref.asset
		events = append(events, notify.Event{
			OrgID:     ref.org,
			ProgramID: ref.program,
			AssetID:   &asset,
			Kind:      notify.KindExternalDead,
			Priority:  notify.Priorities[notify.KindExternalDead],
			Payload: map[string]any{
				"key":           ref.key,
				"external_host": host,
				"apex":          Apex(host),
				"reason":        "the apex no longer resolves, so the domain is re-registrable",
				"summary":       ref.key + " loads from " + host + ", whose domain has expired",
			},
		})
	}

	if len(events) > 0 {
		batch, err := notify.Batch(ref.org, ref.program, s.now(), events)
		if err != nil {
			return 0, err
		}
		if _, err := q.WriteEvents(ctx, batch); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(events), nil
}

// Apex is the registrable domain of a name. Through the public suffix list, not
// "the last two labels" ("example.co.uk" has three): a wrong answer resolves a
// public suffix, which always resolves, and every finding under it silently
// disappears.
func Apex(host string) string {
	if host == "" {
		return ""
	}
	apex, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return apex
}

// decode reads the list the statement returned, through both shapes on purpose.
// The column is jsonb and what a driver returns for it is a decoding decision,
// not a contract (bytes on one path, a decoded slice on another). Asserting the
// wrong one fails silently and reads as "this host is newly dead": a critical
// alert re-sent every tick while the domain stays gone.
func decode(value any) []string {
	var raw []byte
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		raw = typed
	case string:
		raw = []byte(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil
		}
		raw = encoded
	}

	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
