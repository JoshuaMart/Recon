package ct

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/inventory"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// seenCap bounds the cache of names already handled. It only spares the
// database a write: correctness rests on the asset's unique key.
const seenCap = 200_000

// queueSize is how many matching certificates wait for the writer. The
// reader never waits on the database: past this, a certificate is dropped
// and counted, a gap that reads as one.
const queueSize = 2_000

// flushEvery is how often a session's counters are written.
const flushEvery = 30 * time.Second

// Stream reads the certstream feed and turns matching names into
// candidates. A reader decodes and matches in memory; a writer does the
// database work, so a burst of matches never stops the feed from being read.
type Stream struct {
	Store      *store.Store
	URL        string
	Log        *slog.Logger
	Now        func() time.Time
	MaxPerHour int
	Reload     time.Duration

	mu        sync.Mutex
	index     Index
	rules     map[uuid.UUID]*scope.Classifier
	loaded    time.Time
	seen      map[string]struct{}
	hour      time.Time
	perHour   map[uuid.UUID]int
	over      map[string]struct{}
	wildcards map[uuid.UUID]int
	connected bool
	session   int64
	counts    counts
}

type counts struct{ certificates, matched, created, revived, overflow, dropped int64 }

type match struct {
	cert    Certificate
	name    string
	targets []Target
}

func (s *Stream) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

// Connected says whether the feed is being read, for the health check.
func (s *Stream) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}

// Loop reads the feed until ctx ends, reconnecting with a backoff, while the
// writer drains the matches.
func (s *Stream) Loop(ctx context.Context) {
	if err := s.Store.Q.EndOpenCTSessions(ctx, s.now()); err != nil {
		s.Log.ErrorContext(ctx, "ct: closing stale sessions", "err", err)
	}
	if err := s.LoadApexes(ctx); err != nil {
		s.Log.ErrorContext(ctx, "ct: loading the apexes", "err", err)
	}
	work := make(chan match, queueSize)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.write(ctx, work)
	}()
	defer wg.Wait()

	wait := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.connect(ctx, work)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		s.Log.WarnContext(ctx, "ct: feed disconnected", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

// write is the writer: it claims the matched names, reloads the apexes when
// due and flushes the counters, until ctx ends.
func (s *Stream) write(ctx context.Context, work <-chan match) {
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Flush(context.WithoutCancel(ctx))
			if s.now().Sub(s.loadedAt()) >= s.Reload {
				if err := s.LoadApexes(ctx); err != nil {
					s.Log.ErrorContext(ctx, "ct: reloading the apexes", "err", err)
				}
			}
		case m := <-work:
			for _, t := range m.targets {
				if _, err := s.claim(ctx, t.ProgramID, m.name, m.cert); err != nil && ctx.Err() == nil {
					s.Log.ErrorContext(ctx, "ct: writing a candidate", "name", m.name, "err", err)
				}
			}
		}
	}
}

// connect is one connected period, recorded so a gap reads as a gap.
func (s *Stream) connect(ctx context.Context, work chan<- match) error {
	c, _, err := websocket.Dial(ctx, s.URL, nil)
	if err != nil {
		return err
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(4 << 20)
	id, err := s.Store.Q.StartCTSession(ctx, s.now())
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.connected, s.session = true, id
	s.mu.Unlock()
	s.Log.InfoContext(ctx, "ct: feed connected", "url", s.URL)

	err = s.read(ctx, c, work)
	s.mu.Lock()
	s.connected = false
	s.mu.Unlock()
	s.Flush(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.session = 0
	s.mu.Unlock()
	// read only returns on an error: a shutdown is not one worth keeping.
	var msg *string
	if ctx.Err() == nil {
		m := err.Error()
		msg = &m
	}
	if endErr := s.Store.Q.EndCTSession(context.WithoutCancel(ctx), sqlcgen.EndCTSessionParams{ID: id, Now: s.now(), Error: msg}); endErr != nil {
		s.Log.ErrorContext(ctx, "ct: ending the session", "err", endErr)
	}
	return err
}

// read decodes and matches, in memory only. A match the writer has no room
// for is dropped and counted.
func (s *Stream) read(ctx context.Context, c *websocket.Conn, work chan<- match) error {
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return err
		}
		cert, err := Parse(b)
		if errors.Is(err, errNotCertificate) {
			continue
		}
		if err != nil {
			s.Log.DebugContext(ctx, "ct: unreadable message", "err", err)
			continue
		}
		s.mu.Lock()
		s.counts.certificates++
		s.mu.Unlock()
		for _, m := range s.matches(cert) {
			select {
			case work <- m:
			default:
				s.mu.Lock()
				s.counts.dropped++
				s.mu.Unlock()
			}
		}
	}
}

// matches lists the names of a certificate to write. Wildcard names are only
// counted, once per wildcard and name while the cache holds them.
func (s *Stream) matches(cert Certificate) []match {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []match
	for _, san := range cert.Names {
		name, targets, wildcard := s.index.Match(san)
		if len(targets) == 0 {
			continue
		}
		if wildcard {
			for _, t := range targets {
				if s.remember("w|" + t.WildcardID.String() + "|" + name) {
					if s.wildcards == nil {
						s.wildcards = map[uuid.UUID]int{}
					}
					s.wildcards[t.WildcardID]++
				}
			}
			continue
		}
		out = append(out, match{cert: cert, name: name, targets: targets})
	}
	return out
}

// remember adds a key to the seen cache and says whether it was new. The
// caller holds the lock.
func (s *Stream) remember(key string) bool {
	if _, ok := s.seen[key]; ok {
		return false
	}
	if s.seen == nil || len(s.seen) >= seenCap {
		s.seen = map[string]struct{}{}
	}
	s.seen[key] = struct{}{}
	return true
}

func (s *Stream) loadedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

// Flush writes the counters: the session's, and the wildcard names counted
// per wildcard.
func (s *Stream) Flush(ctx context.Context) {
	s.mu.Lock()
	c, w, session := s.counts, s.wildcards, s.session
	s.counts, s.wildcards = counts{}, nil
	s.mu.Unlock()
	for id, n := range w {
		if err := s.Store.Q.CountWildcardSAN(ctx, sqlcgen.CountWildcardSANParams{ID: id, N: int32(n)}); err != nil { //nolint:gosec // a count of names
			s.Log.ErrorContext(ctx, "ct: counting wildcard names", "err", err)
		}
	}
	if session == 0 {
		return
	}
	if err := s.Store.Q.CountCTSession(ctx, sqlcgen.CountCTSessionParams{
		ID: session, Certificates: c.certificates, Matched: c.matched, Created: c.created,
		Revived: c.revived, Overflow: c.overflow, Dropped: c.dropped,
	}); err != nil {
		s.Log.ErrorContext(ctx, "ct: counting the session", "err", err)
	}
}

// LoadApexes reads the apexes and each program's rules again: a wildcard
// added or a program paused is followed within Reload.
func (s *Stream) LoadApexes(ctx context.Context) error {
	rows, err := s.Store.Q.CTApexes(ctx)
	if err != nil {
		return err
	}
	ix := Index{}
	rules := map[uuid.UUID]*scope.Classifier{}
	for _, r := range rows {
		ix.Add(Target{WildcardID: r.ID, ProgramID: r.ProgramID, Apex: r.Apex})
		if _, ok := rules[r.ProgramID]; ok {
			continue
		}
		c, _, err := inventory.Rules(ctx, s.Store.Q, r.ProgramID)
		if err != nil {
			return err
		}
		rules[r.ProgramID] = c
	}
	s.mu.Lock()
	s.index, s.rules, s.loaded = ix, rules, s.now()
	s.mu.Unlock()
	return nil
}

// Handle writes what one certificate brings, synchronously: the writer's
// work for one certificate, for tests and tools.
func (s *Stream) Handle(ctx context.Context, cert Certificate) error {
	for _, m := range s.matches(cert) {
		for _, t := range m.targets {
			if _, err := s.claim(ctx, t.ProgramID, m.name, cert); err != nil {
				return err
			}
		}
	}
	return nil
}

// admit applies the guards: a name already handled is skipped, and a
// program past its hourly ceiling counts the overflow, once per name. The
// name is remembered only once written, by done.
func (s *Stream) admit(program uuid.UUID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[key]; ok {
		return false
	}
	if h := s.now().Truncate(time.Hour); !h.Equal(s.hour) {
		s.hour, s.perHour, s.over = h, map[uuid.UUID]int{}, map[string]struct{}{}
	}
	if s.perHour[program] >= s.MaxPerHour {
		if _, ok := s.over[key]; !ok {
			s.over[key] = struct{}{}
			s.counts.overflow++
		}
		return false
	}
	s.counts.matched++
	s.perHour[program]++
	return true
}

// done records the outcome of an admitted name: written, it is remembered;
// failed, its place under the ceiling is given back so the next sighting
// tries again.
func (s *Stream) done(program uuid.UUID, key string, written bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if written {
		s.remember(key)
		return
	}
	s.counts.matched--
	s.perHour[program]--
}

// claim writes one name for one program, in its own transaction, and says
// whether it entered the CT lane.
func (s *Stream) claim(ctx context.Context, program uuid.UUID, name string, cert Certificate) (bool, error) {
	key := program.String() + "|" + name
	if !s.admit(program, key) {
		return false, nil
	}
	entered, revived, err := s.writeName(ctx, program, name, cert)
	s.done(program, key, err == nil)
	if err != nil || !entered {
		return false, err
	}
	s.mu.Lock()
	if revived {
		s.counts.revived++
	} else {
		s.counts.created++
	}
	s.mu.Unlock()
	s.Log.InfoContext(ctx, "ct: candidate", "name", name, "program", program, "revived", revived)
	return true, nil
}

func (s *Stream) writeName(ctx context.Context, program uuid.UUID, name string, cert Certificate) (entered, revived bool, err error) {
	kind, key := inventory.HostAsset(name)
	if kind != inventory.KindFQDN {
		return false, false, nil
	}
	s.mu.Lock()
	classifier := s.rules[program]
	s.mu.Unlock()
	if classifier == nil {
		return false, false, fmt.Errorf("no rules loaded for program %s", program)
	}

	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Store.Q.WithTx(tx)
	now := s.now()
	w := &inventory.Writer{Q: q, ProgramID: program, Classifier: classifier, Now: now, ProgramActive: true}
	wr, err := w.Upsert(ctx, inventory.Asset{
		ProgramID: program, Kind: kind, Key: key, Host: key, Source: "certstream", Lifecycle: "candidate",
		Path: []map[string]any{{"step": "certificate", "issuer": cert.Issuer, "log": cert.Log, "seen": cert.Seen}},
	})
	if err != nil {
		return false, false, err
	}
	if wr.Scope != "in_scope" {
		return false, false, tx.Commit(ctx)
	}
	if !wr.New {
		cur, err := q.GetAssetCurrent(ctx, wr.ID)
		if err != nil {
			return false, false, err
		}
		// Known and alive: the unique key absorbs it. Archived: a new
		// certificate is the strongest sign it is coming back.
		if cur.Lifecycle != "archived" {
			return false, false, tx.Commit(ctx)
		}
		if err := w.SetLifecycle(ctx, wr.ID, "candidate"); err != nil {
			return false, false, err
		}
		revived = true
	}
	if _, err := q.EnterCTLane(ctx, sqlcgen.EnterCTLaneParams{AssetID: wr.ID, Now: now}); err != nil {
		return false, false, err
	}
	return true, revived, tx.Commit(ctx)
}
