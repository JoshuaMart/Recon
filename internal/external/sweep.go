// Package external answers the one question the inventory cannot: is somebody
// else's domain still registered.
//
// It exists because the dangerous half of external_host_dead is the one the
// cross reference cannot see. A host in the organization's own inventory has a
// lifecycle and dies visibly; a genuine third party whose domain expired is
// re-registrable by anyone, which is the classic supply chain case, and nothing
// in this system would ever have looked at it.
//
// What it may do is strictly bounded, and the bound is what makes the permission
// defensible: it resolves the name and its apex, and it never opens a TCP
// connection, never sends an HTTP request and never renders. A query to a public
// resolver reaches neither the domain nor its infrastructure, which is the same
// reasoning that gives a resolution no cost against a programme's budget.
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

// Resolver answers whether an apex is still registered.
//
// An interface because the alternative is a test that depends on somebody's
// expired domain staying expired, which is a test that goes red on a Tuesday
// for a reason nobody can act on.
type Resolver interface {
	// Registered is false only on an authoritative "no such domain". A timeout,
	// a refusal or a broken resolver are none of the three, and treating them
	// as an expiry would raise a critical alert on every asset of every tenant
	// the first time the local resolver had a bad minute.
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

// Registered asks for the apex's name servers.
//
// NS rather than A, and the difference decides the finding. A registered domain
// parked with no address still has name servers; an expired one answers
// NXDOMAIN to everything. Asking for an address would read a parked domain as
// re-registrable, which is a critical alert about nothing.
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

// New builds it.
//
// On the system pool, because it serves every tenant in one tick and the set of
// third party hosts is deduplicated across all of them: the same public CDN
// appears in every inventory and is worth one lookup rather than one per tenant.
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
}

// Once resolves every distinct apex once and writes what it found.
func (s *Sweep) Once(ctx context.Context) error {
	queries := sqlcgen.New(s.pool)

	rows, err := queries.ThirdPartyHosts(ctx, sqlcgen.ThirdPartyHostsParams{Cap: int32(s.batch)}) //nolint:gosec // bounded by configuration
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	references := make([]reference, 0, len(rows))
	apexes := map[string]bool{}
	for _, row := range rows {
		host, _ := row.Host.(string)
		apex := Apex(host)
		if apex == "" {
			continue
		}
		references = append(references, reference{
			asset:   uuid.UUID(row.AssetID.Bytes),
			org:     uuid.UUID(row.OrgID.Bytes),
			program: uuid.UUID(row.ProgramID.Bytes),
			key:     row.Key,
			host:    host,
		})
		apexes[apex] = false
	}

	// One lookup per distinct apex, which is dozens for a whole deployment
	// rather than one per reference. The host itself is never resolved: a name
	// that no longer answers under a domain that is still registered is a
	// dangling subdomain at somebody else's, which is not re-registrable and not
	// the same finding.
	gone := map[string]bool{}
	for apex := range apexes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		registered, err := s.resolver.Registered(ctx, apex)
		if err != nil {
			// Not a verdict. A resolver having a bad minute is not a domain
			// expiring, and the next tick asks again.
			s.log.WarnContext(ctx, "could not resolve a third party apex", "apex", apex, "error", err)
			continue
		}
		gone[apex] = !registered
	}

	// Grouped per asset, because the verdict is written onto the asset as one
	// list. Writing it per reference would have the last host of an asset erase
	// the others.
	dead := map[uuid.UUID][]string{}
	owner := map[uuid.UUID]reference{}
	for _, ref := range references {
		owner[ref.asset] = ref
		if gone[Apex(ref.host)] {
			dead[ref.asset] = append(dead[ref.asset], ref.host)
		}
	}
	for asset := range owner {
		if _, listed := dead[asset]; !listed {
			// An empty list is written too, and that is not a no-op: it is how
			// a domain somebody re-registered stops being reported.
			dead[asset] = nil
		}
	}

	found := 0
	for asset, hosts := range dead {
		emitted, err := s.record(ctx, queries, owner[asset], hosts)
		if err != nil {
			return err
		}
		found += emitted
	}
	if found > 0 {
		s.log.InfoContext(ctx, "third party hosts found unregistered",
			"references", len(references), "apexes", len(apexes), "events", found)
	}
	return nil
}

// record writes the verdict and tells only what changed.
func (s *Sweep) record(
	ctx context.Context, queries *sqlcgen.Queries, ref reference, hosts []string,
) (int, error) {
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

// Apex is the registrable domain of a name.
//
// Through the public suffix list rather than "the last two labels", because
// "example.co.uk" has three and "example.com" has two, and getting it wrong
// means resolving a public suffix, which always resolves, so every finding
// under it silently disappears.
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

// decode reads the list the statement returned.
//
// Through both shapes on purpose. The column is jsonb, and what a driver hands
// back for one is a decoding decision rather than a contract: bytes on one path
// and a decoded slice on another. A type assertion on the one that happened to
// come back first would fail silently on the other, and the failure here reads
// as "this host is newly dead", which is a critical alert re-sent on every tick
// for as long as the domain stays gone.
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
