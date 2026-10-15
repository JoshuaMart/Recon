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

// Options are the matcher's tunables, all configuration.
type Options struct {
	// Interval is the tick that reloads the set and flushes the counters. It also
	// sets the feed's presence granularity: above a minute that record is coarser
	// than its table.
	Interval time.Duration
	// Ceiling is how many candidates one programme may create per window.
	Ceiling int
	// Window is what the ceiling is per.
	Window time.Duration
	// CacheTTL is how long a name stays deduplicated in memory.
	CacheTTL time.Duration
	// CacheSize bounds the cache; an unbounded map on a stream is a slow leak.
	CacheSize int
}

// DefaultOptions are what a deployment gets by default. The ceiling is generous
// for a real perimeter and tight against a mistake: forty new names an hour is
// unusual, four thousand is an apex rule pointing at someone else's shared
// infrastructure.
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
// The set is read lock-free through an atomic pointer swapped whole on reload.
// The hot path's accumulators sit behind one mutex, which costs nothing
// measurable at a few thousand frames a second and removes a class of bug
// showing up as inexplicable counters.
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
	dropped   int64
}

// New builds a matcher with an empty set: it matches nothing until the first
// reload, so a stream arriving before the perimeter is known creates nothing.
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

// Swap replaces the set. Reload calls it, and tests use it to prove lock-free
// reads: a map written during a walk gives wrong matches, not an error, the one
// silent fault here.
func (m *Matcher) Swap(set *Set) { m.set.Store(set) }

// Apexes is how many apexes the current set holds.
func (m *Matcher) Apexes() int { return m.set.Load().Apexes() }

// Reload rebuilds the apex set and reconciles the per-apex rows against it. The
// set is built whole and swapped, as the stream reads it without a lock.
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
		// The normalized spelling, for set and rows alike. Raw, WatchApexes created a row
		// under the typed pattern while counters landed under the canonical one, and
		// the next reload deleted the counted row: coverage read zero for that apex,
		// silently, once a minute.
		apex, ok := NormalizeApex(row.Apex)
		if !ok {
			m.log.WarnContext(ctx, "an apex rule is not a usable name and is watched by nothing",
				"program", uuid.UUID(row.ProgramID.Bytes), "pattern", row.Apex)
			continue
		}
		claims = append(claims, Claim{
			OrgID:     uuid.UUID(row.OrgID.Bytes),
			ProgramID: uuid.UUID(row.ProgramID.Bytes),
			Apex:      apex,
		})
		orgs = append(orgs, row.OrgID)
		programs = append(programs, row.ProgramID)
		apexes = append(apexes, apex)
	}

	m.Swap(NewSet(claims))

	q := sqlcgen.New(m.sys)
	// The row comes first, so an apex watched from now on and silent differs from
	// one nothing watches.
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

// Handle walks one frame and writes whatever it reveals. Counters move for
// every sighting, including those the cache and ceiling then refuse: what an
// apex delivered is a fact about the logs, and counting only what was created
// would make coverage a report on this process.
func (m *Matcher) Handle(ctx context.Context, frame *Frame) {
	set := m.set.Load()

	var sightings []Sighting
	var malformed int
	if frame.MessageType == MessageCertificate {
		sightings, malformed = set.Sightings(frame.Data.LeafCert.AllDomains)
	}

	// Nearly every certificate matches nothing: count it and skip the maps below.
	if len(sightings) == 0 {
		m.mu.Lock()
		m.frames++
		m.malformed += int64(malformed)
		m.mu.Unlock()
		return
	}

	at := m.now()
	byProgram := map[uuid.UUID]*pending{}

	// One name is written once per programme, whatever produced it twice: a programme holding an
	// apex and something under it claims a name through each (the walk returns both), and two
	// spellings in one certificate survive Sightings, which dedupes on the raw string. Otherwise
	// both spend the ceiling twice and enter the key twice. Counters still move per claim: they
	// are about the apex, not what was written.
	written := make(map[counterKey]struct{}, len(sightings))

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

		once := counterKey{program: sighting.Claim.ProgramID, apex: sighting.Name}
		if _, already := written[once]; already {
			continue
		}
		written[once] = struct{}{}

		if m.cache.seen(sighting.Claim.ProgramID, sighting.Name, at) {
			continue
		}
		allowed, firstRefusal := m.budgetFor(sighting.Claim.ProgramID, at).take()
		if !allowed {
			// Counted against the apex as well as logged: the dropped count must be readable,
			// and grepping a control plane is not that.
			counter.dropped++
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

// write records one certificate's names for one programme, scoped on the
// application pool under the organization of the matched apex. Only the set
// reload crosses tenants.
//
// The perimeter is read here, not held with the set, because rules may have
// changed since the last reload. A match is rare, so the round trip is paid on
// occasions that matter, not on the stream.
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

// certificateOf is the lineage a candidate keeps. The issuer's organization,
// not its common name: "Let's Encrypt" is what a reader recognises, "YE1" is the
// intermediate's label. The common name is the fallback for issuers with no
// organization.
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
// The one place allowed to lose a minute: a write per certificate is the round
// trip the cache removes, and a crash loses the unflushed window. Acceptable
// because a counter is a metric, not the journal; an asset created from a
// certificate was written immediately.
func (m *Matcher) Flush(ctx context.Context) error {
	at := m.now()

	m.mu.Lock()
	counters, frames := m.counters, m.frames
	malformed, created := m.malformed, m.created
	m.counters, m.frames, m.malformed, m.created = map[counterKey]*counts{}, 0, 0, 0
	m.mu.Unlock()

	// One line per tick, not per certificate: a busy apex creates hundreds a minute,
	// and a line each buries the record of the work.
	if created > 0 {
		m.log.InfoContext(ctx, "certificate transparency candidates",
			"created", created, "frames", frames)
	}

	q := sqlcgen.New(m.sys)

	if malformed > 0 {
		m.log.WarnContext(ctx, "SANs that were not names",
			"count", malformed, "frames", frames)
	}

	// Counters go first, and a failure puts them back instead of dropping them: a
	// transient error on either statement once lost a whole window, including the
	// dropped figure the migration made readable. A crash may lose a minute; a
	// deadlock must not.
	if len(counters) > 0 {
		if err := m.bump(ctx, q, at, counters); err != nil {
			m.restore(counters, frames)
			return err
		}
	}

	if frames > 0 {
		if err := q.RecordFeedMinute(ctx, sqlcgen.RecordFeedMinuteParams{
			At: stamp(at), Frames: frames,
		}); err != nil {
			// Counters are already written, so only the presence record is put back. A minute
			// saying nothing arrived is the reading this table must never produce by accident.
			m.restore(nil, frames)
			return fmt.Errorf("record the feed minute: %w", err)
		}
	}
	return nil
}

// restore folds a failed flush back into the accumulator. Merged, not assigned:
// the stream kept running during the write, so newer counts may exist.
func (m *Matcher) restore(counters map[counterKey]*counts, frames int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.frames += frames
	for key, count := range counters {
		current := m.counters[key]
		if current == nil {
			m.counters[key] = count
			continue
		}
		current.sans += count.sans
		current.wildcards += count.wildcards
		current.dropped += count.dropped
	}
}

func (m *Matcher) bump(
	ctx context.Context, q *sqlcgen.Queries, at time.Time, counters map[counterKey]*counts,
) error {
	orgs := make([]pgtype.UUID, 0, len(counters))
	programs := make([]pgtype.UUID, 0, len(counters))
	apexes := make([]string, 0, len(counters))
	sans := make([]int64, 0, len(counters))
	wildcards := make([]int64, 0, len(counters))
	dropped := make([]int64, 0, len(counters))
	for key, count := range counters {
		orgs = append(orgs, uuidTo(count.org))
		programs = append(programs, uuidTo(key.program))
		apexes = append(apexes, key.apex)
		sans = append(sans, count.sans)
		wildcards = append(wildcards, count.wildcards)
		dropped = append(dropped, count.dropped)
	}

	if err := q.BumpApexCounters(ctx, sqlcgen.BumpApexCountersParams{
		OrgIds: orgs, ProgramIds: programs, Apexes: apexes, At: stamp(at),
		Sans: sans, Wildcards: wildcards, Dropped: dropped,
	}); err != nil {
		return fmt.Errorf("bump the apex counters: %w", err)
	}
	return nil
}

// undecodable counts a message that was not JSON. Counted, not logged: at a few
// thousand frames a second a line each would be the outage.
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
