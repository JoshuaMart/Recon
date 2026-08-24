package ct

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Options are the tunables of the matcher. Every one of them is configuration
// rather than a constant, which is what P5 asks of anything of this kind.
type Options struct {
	// Interval is the tick that reloads the set and flushes the counters. It
	// also decides the granularity of the feed's presence record, so a value
	// above a minute makes that record coarser than the table it writes to.
	Interval time.Duration
	// Ceiling is how many candidates one programme may create per window.
	Ceiling int
	// Window is what the ceiling is per.
	Window time.Duration
	// CacheTTL is how long a name stays deduplicated in memory.
	CacheTTL time.Duration
	// CacheSize bounds the cache. An unbounded map on a stream is a leak with
	// a slow fuse.
	CacheSize int
}

// DefaultOptions are the ones a deployment gets without saying anything.
//
// The ceiling is deliberately generous against a real perimeter and tight
// against a mistake: a programme legitimately seeing forty new names in an hour
// is unusual, and one seeing four thousand is an apex rule pointing at somebody
// else's shared infrastructure.
func DefaultOptions() Options {
	return Options{
		Interval:  time.Minute,
		Ceiling:   500,
		Window:    time.Hour,
		CacheTTL:  10 * time.Minute,
		CacheSize: 50_000,
	}
}

// Matcher walks the stream against the apex set and writes what it finds.
//
// The set is read without a lock, through an atomic pointer swapped whole by a
// reload. Everything the hot path accumulates sits behind one mutex, which at a
// few thousand frames a second costs nothing measurable and removes a class of
// bug that would show up as counters nobody can explain.
type Matcher struct {
	sys      *pgxpool.Pool
	app      *store.Scoped
	ingestor *ingest.Ingestor
	log      *slog.Logger
	now      func() time.Time
	opts     Options

	set atomic.Pointer[Set]

	mu        sync.Mutex
	cache     *cache
	budgets   map[uuid.UUID]*budget
	counters  map[counterKey]*counts
	frames    int64
	malformed int64
	created   int64
}

type counterKey struct {
	program uuid.UUID
	apex    string
}

type counts struct {
	org       uuid.UUID
	sans      int64
	wildcards int64
}

// New builds a matcher. It starts with an empty set, so it matches nothing
// until the first reload: a stream arriving before the perimeter is known must
// create nothing rather than guess.
func New(
	sys *pgxpool.Pool, app *store.Scoped, ingestor *ingest.Ingestor,
	opts Options, log *slog.Logger,
) *Matcher {
	if opts.Interval <= 0 {
		opts = DefaultOptions()
	}
	m := &Matcher{
		sys: sys, app: app, ingestor: ingestor, log: log,
		now:      time.Now,
		opts:     opts,
		cache:    newCache(opts.CacheTTL, opts.CacheSize),
		budgets:  map[uuid.UUID]*budget{},
		counters: map[counterKey]*counts{},
	}
	m.set.Store(NewSet(nil))
	return m
}

// WithClock replaces the clock, for tests that own time.
func (m *Matcher) WithClock(now func() time.Time) *Matcher {
	m.now = now
	m.cache.now = now
	return m
}

// Apexes is how many apexes the current set holds.
func (m *Matcher) Apexes() int { return m.set.Load().Apexes() }

// Reload rebuilds the apex set and reconciles the per apex rows against it.
//
// The set is built whole and swapped rather than mutated, because the stream
// reads it without a lock and a map being written under a walk is the one fault
// here that produces wrong matches instead of an error.
func (m *Matcher) Reload(ctx context.Context) error {
	at := m.now()

	rows, err := sqlcgen.New(m.sys).ApexSet(ctx, sqlcgen.ApexSetParams{At: stamp(at)})
	if err != nil {
		return fmt.Errorf("read the apex set: %w", err)
	}

	claims := make([]Claim, 0, len(rows))
	orgs := make([]pgtype.UUID, 0, len(rows))
	programs := make([]pgtype.UUID, 0, len(rows))
	apexes := make([]string, 0, len(rows))
	for _, row := range rows {
		claims = append(claims, Claim{
			OrgID:     uuid.UUID(row.OrgID.Bytes),
			ProgramID: uuid.UUID(row.ProgramID.Bytes),
			Apex:      row.Apex,
		})
		orgs = append(orgs, row.OrgID)
		programs = append(programs, row.ProgramID)
		apexes = append(apexes, row.Apex)
	}

	m.set.Store(NewSet(claims))

	q := sqlcgen.New(m.sys)
	// The row comes first, so that an apex watched from now on and silent is
	// distinguishable from an apex nothing is watching.
	if err := q.WatchApexes(ctx, sqlcgen.WatchApexesParams{
		OrgIds: orgs, ProgramIds: programs, Apexes: apexes, At: stamp(at),
	}); err != nil {
		return fmt.Errorf("record the watched apexes: %w", err)
	}
	forgotten, err := q.ForgetApexesOutsideTheSet(ctx, sqlcgen.ForgetApexesOutsideTheSetParams{
		ProgramIds: programs, Apexes: apexes,
	})
	if err != nil {
		return fmt.Errorf("forget the apexes outside the set: %w", err)
	}
	if forgotten > 0 {
		m.log.InfoContext(ctx, "apexes left the certificate transparency set",
			"forgotten", forgotten, "watching", len(apexes))
	}
	return nil
}

// Handle walks one frame and writes whatever it reveals.
//
// The order is deliberate. Counters move for every sighting, including the ones
// the cache and the ceiling then refuse to write: what an apex delivered is a
// fact about the logs, and reading it off what happened to be created would
// make the coverage metric a report on this process rather than on the stream.
func (m *Matcher) Handle(ctx context.Context, frame *Frame) {
	set := m.set.Load()

	var sightings []Sighting
	var malformed int
	if frame.MessageType == MessageCertificate {
		sightings, malformed = set.Sightings(frame.Data.LeafCert.AllDomains)
	}

	at := m.now()
	byProgram := map[uuid.UUID]*pending{}

	m.mu.Lock()
	m.frames++
	m.malformed += int64(malformed)
	for _, sighting := range sightings {
		counter := m.counterFor(sighting.Claim)
		if sighting.Wildcard {
			counter.wildcards++
			continue
		}
		counter.sans++

		if m.cache.seen(sighting.Claim.ProgramID, sighting.Name, at) {
			continue
		}
		allowed, firstRefusal := m.budgetFor(sighting.Claim.ProgramID, at).take()
		if !allowed {
			if firstRefusal {
				m.log.Warn("a programme reached its certificate transparency ceiling",
					"program", sighting.Claim.ProgramID, "apex", sighting.Claim.Apex,
					"ceiling", m.opts.Ceiling, "window", m.opts.Window)
			}
			continue
		}

		batch := byProgram[sighting.Claim.ProgramID]
		if batch == nil {
			batch = &pending{org: sighting.Claim.OrgID}
			byProgram[sighting.Claim.ProgramID] = batch
		}
		batch.names = append(batch.names, sighting.Name)
	}
	m.mu.Unlock()

	for program, batch := range byProgram {
		if err := m.write(ctx, program, batch, frame); err != nil {
			m.log.ErrorContext(ctx, "writing candidates failed",
				"program", program, "names", len(batch.names), "error", err)
			continue
		}
		m.mu.Lock()
		for _, name := range batch.names {
			m.cache.record(program, name, at)
		}
		m.mu.Unlock()
	}
}

type pending struct {
	org   uuid.UUID
	names []string
}

// write records one certificate's names for one programme.
//
// Scoped, on the application pool, under the organization the matched apex
// named. The set reload is the query that crosses tenants; everything it hands
// back is written inside one.
//
// The perimeter is read here rather than held with the set, because a rule may
// have changed since the last reload and the scope is re-evaluated at every
// write by design. A match is rare, so the extra round trip is paid on the
// occasions that matter and never on the stream.
func (m *Matcher) write(ctx context.Context, program uuid.UUID, batch *pending, frame *Frame) error {
	tx, err := m.app.Begin(ctx, batch.org)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	at := m.now()
	perimeter, err := ingest.CompileScope(ctx, q, program, at)
	if err != nil {
		return fmt.Errorf("compile the perimeter: %w", err)
	}

	entered, err := m.ingestor.EnterCandidates(ctx, q, ingest.Run{
		ID:          uuid.New(),
		OrgID:       batch.org,
		ProgramID:   program,
		Source:      ingest.SourceCertstream,
		Certificate: certificateOf(frame),
	}, perimeter, batch.names)
	if err != nil {
		return fmt.Errorf("enter candidates: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	created := 0
	for _, accepted := range entered.Accepted {
		if accepted.Created {
			created++
		}
	}
	if created > 0 {
		m.mu.Lock()
		m.created += int64(created)
		m.mu.Unlock()
	}
	return nil
}

// certificateOf is the lineage a candidate keeps.
//
// The issuer's organization rather than its common name, because "Let's
// Encrypt" is what somebody reading a lineage recognises and "YE1" is the
// intermediate's label. The common name is the fallback rather than the
// preference, for the issuers that carry no organization.
func certificateOf(frame *Frame) *ingest.Certificate {
	issuer := frame.Data.LeafCert.Issuer.O
	if issuer == "" {
		issuer = frame.Data.LeafCert.Issuer.CN
	}
	return &ingest.Certificate{
		Issuer: issuer,
		Log:    frame.Data.Source.Name,
		Index:  frame.Data.CertIndex,
	}
}

// Flush writes the accumulated counters and says the feed was alive.
//
// This is the one place in the system allowed to lose a minute. A write per
// certificate is the round trip the cache exists to remove, and a crash loses
// the unflushed window. That is acceptable because a counter is a metric and
// not the journal: an asset created from a certificate was written immediately
// and is never in this window.
func (m *Matcher) Flush(ctx context.Context) error {
	at := m.now()

	m.mu.Lock()
	counters, frames := m.counters, m.frames
	malformed, created := m.malformed, m.created
	m.counters, m.frames, m.malformed, m.created = map[counterKey]*counts{}, 0, 0, 0
	m.mu.Unlock()

	// One line per tick rather than one per certificate. A busy apex creates
	// hundreds in a minute, and a line each turns the record of the work into
	// the reason nobody reads the log. This is the same correction the
	// rendering service made about page slot contention.
	if created > 0 {
		m.log.InfoContext(ctx, "certificate transparency candidates",
			"created", created, "frames", frames)
	}

	q := sqlcgen.New(m.sys)

	if frames > 0 {
		if err := q.RecordFeedMinute(ctx, sqlcgen.RecordFeedMinuteParams{
			At: stamp(at), Frames: frames,
		}); err != nil {
			return fmt.Errorf("record the feed minute: %w", err)
		}
	}
	if malformed > 0 {
		m.log.WarnContext(ctx, "SANs that were not names",
			"count", malformed, "frames", frames)
	}
	if len(counters) == 0 {
		return nil
	}

	orgs := make([]pgtype.UUID, 0, len(counters))
	programs := make([]pgtype.UUID, 0, len(counters))
	apexes := make([]string, 0, len(counters))
	sans := make([]int64, 0, len(counters))
	wildcards := make([]int64, 0, len(counters))
	for key, count := range counters {
		orgs = append(orgs, uuidTo(count.org))
		programs = append(programs, uuidTo(key.program))
		apexes = append(apexes, key.apex)
		sans = append(sans, count.sans)
		wildcards = append(wildcards, count.wildcards)
	}

	if err := q.BumpApexCounters(ctx, sqlcgen.BumpApexCountersParams{
		OrgIds: orgs, ProgramIds: programs, Apexes: apexes, At: stamp(at),
		Sans: sans, Wildcards: wildcards,
	}); err != nil {
		return fmt.Errorf("bump the apex counters: %w", err)
	}
	return nil
}

// undecodable counts a message that was not JSON at all.
//
// Counted rather than logged per occurrence: at a few thousand frames a second,
// a line each would be the outage rather than the record of one.
func (m *Matcher) undecodable() {
	m.mu.Lock()
	m.malformed++
	m.mu.Unlock()
}

// counterFor must be called under the mutex.
func (m *Matcher) counterFor(claim Claim) *counts {
	key := counterKey{program: claim.ProgramID, apex: claim.Apex}
	count := m.counters[key]
	if count == nil {
		count = &counts{org: claim.OrgID}
		m.counters[key] = count
	}
	return count
}

func stamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: true}
}

func uuidTo(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
