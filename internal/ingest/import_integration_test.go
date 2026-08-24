//go:build integration

// What an import may and may not do, asserted against a database rather than
// against the structure the decoder returned. Every claim 7.6 makes about
// writes lands here.
package ingest_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/bbot"
	"github.com/JoshuaMart/recon/internal/ingest"
)

// realScan is the slice of a scan the tool actually wrote. Every field shape it
// exercises was a guess until this file contradicted one of them.
func realScan(t *testing.T) bbot.Scan {
	t.Helper()

	file, err := os.Open("../bbot/testdata/scan.ndjson")
	if err != nil {
		t.Fatalf("open the fixture: %v", err)
	}
	defer func() { _ = file.Close() }()

	scan, err := bbot.Parse(file)
	if err != nil {
		t.Fatalf("parse the fixture: %v", err)
	}
	return scan
}

func (h *harness) importRun() ingest.Run {
	return ingest.Run{ID: uuid.New(), OrgID: h.org, ProgramID: h.program, Kind: "import"}
}

// stream builds a scan from lines a test wrote, for the cases a real file does
// not happen to contain.
func stream(t *testing.T, lines ...string) bbot.Scan {
	t.Helper()

	scan, err := bbot.Parse(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return scan
}

// firstSeen reads every asset's date, keyed by its canonical key.
func (h *harness) firstSeen(t *testing.T) map[string]time.Time {
	t.Helper()

	rows, err := h.pool.Query(context.Background(),
		`SELECT key, first_seen FROM asset WHERE program_id = $1`, h.program)
	if err != nil {
		t.Fatalf("read first_seen: %v", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var key string
		var at time.Time
		if err := rows.Scan(&key, &at); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[key] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func (h *harness) nullableDue(t *testing.T, column, key string) *time.Time {
	t.Helper()

	var due *time.Time
	err := h.pool.QueryRow(context.Background(),
		`SELECT `+column+` FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, key).Scan(&due)
	if err != nil {
		t.Fatalf("read %s of %s: %v", column, key, err)
	}
	return due
}

// An import declares and never observes. This is the assertion the whole shape
// of 7.6 rests on, and it is cheap to state and easy to lose: any later change
// that starts writing what the file measured fails here first.
func TestAnImportWritesNoObservation(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	before := h.count(t, `SELECT count(*) FROM observation`)
	imported, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	after := h.count(t, `SELECT count(*) FROM observation`)

	if before != after {
		t.Errorf("observations went from %d to %d, and an import measures nothing", before, after)
	}
	if imported.Assets.Created == 0 {
		t.Fatal("nothing was created, so the assertion above proves nothing")
	}
	if n := h.count(t, `SELECT count(*) FROM asset_layer`); n != 0 {
		t.Errorf("asset_layer has %d rows, and an import has no verdict about a layer", n)
	}
}

// The perimeter classifies, the file does not. A name the tool called
// in-scope-adjacent is stored, visible, and never probed, which is the outcome
// worth having: unknown is where an acquisition shows up.
func TestTheToolsScopeDoesNotDecideAnything(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t)); err != nil {
		t.Fatalf("import: %v", err)
	}

	// dns101.ovh.net is in the fixture, tagged affiliate by the tool, and
	// matches no rule of this programme.
	var status string
	err := h.pool.QueryRow(context.Background(),
		`SELECT scope_status FROM asset WHERE program_id = $1 AND key = $2`,
		h.program, "dns101.partner2.test").Scan(&status)
	if err != nil {
		t.Fatalf("the affiliate name was not stored at all: %v", err)
	}
	if status != "unknown" {
		t.Errorf("scope_status = %q, want unknown", status)
	}
	if due := h.nullableDue(t, "next_resolve_at", "dns101.partner2.test"); due != nil {
		t.Errorf("an out of perimeter name is due at %s, and nothing may probe it", due)
	}

	// And the other direction, so the test above is not passing on a
	// classification that refuses everything.
	if due := h.nullableDue(t, "next_resolve_at", "acme-corp.test"); due == nil {
		t.Error("the in-scope seed has no due date, so nothing will go and look")
	}
}

// The safety property, stated from the direction that matters: a file for a
// perimeter this programme has no rule for creates assets and schedules none.
func TestAScanOfSomebodyElsesPerimeterSchedulesNothing(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("example.com"))

	imported, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if imported.Assets.Created == 0 {
		t.Fatal("nothing was stored, and the point is that it is stored and not probed")
	}
	if imported.Assets.Scheduled != 0 {
		t.Errorf("%d assets were scheduled from a scan of a perimeter with no rule",
			imported.Assets.Scheduled)
	}
	if n := h.count(t,
		`SELECT count(*) FROM asset_current WHERE program_id = $1
		   AND (next_resolve_at IS NOT NULL OR next_full_at IS NOT NULL)`, h.program); n != 0 {
		t.Errorf("%d rows carry a due date, and a run would take them", n)
	}
	if got := imported.Assets.ByScope["in_scope"]; got != 0 {
		t.Errorf("%d assets were classified in scope by a perimeter that names none of them", got)
	}
}

// A claim earns the cheap rung. An import of names that all died last spring
// costs one resolver round trip each and no port sweep at all.
func TestAnImportedNameIsDueForResolveAndNotForFull(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}

	if _, err := h.dated(c).Import(context.Background(), h.queries, h.importRun(), set, realScan(t)); err != nil {
		t.Fatalf("import: %v", err)
	}

	if due := h.nullableDue(t, "next_resolve_at", "acme-corp.test"); due == nil {
		t.Fatal("no resolve date, so nothing checks whether the claim is still true")
	}
	if due := h.nullableDue(t, "next_full_at", "acme-corp.test"); due != nil {
		t.Errorf("next_full_at is %s, and an imported claim has not earned the expensive rung", due)
	}
}

// A service rides its host. One given a date of its own sits in a queue nothing
// dispatches from, which is silent and total.
func TestAnImportedServiceCarriesNoDueDateAndKnowsItsHost(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t)); err != nil {
		t.Fatalf("import: %v", err)
	}

	n := h.count(t,
		`SELECT count(*) FROM asset_current WHERE program_id = $1 AND kind = 'service'
		   AND (next_resolve_at IS NOT NULL OR next_full_at IS NOT NULL)`, h.program)
	if n != 0 {
		t.Errorf("%d services carry a due date of their own", n)
	}

	orphans := h.count(t,
		`SELECT count(*) FROM asset WHERE program_id = $1 AND kind = 'service'
		   AND parent_asset_id IS NULL`, h.program)
	if orphans != 0 {
		t.Errorf("%d services have no host, and their scheduling is carried by nothing", orphans)
	}
	if total := h.count(t,
		`SELECT count(*) FROM asset WHERE program_id = $1 AND kind = 'service'`, h.program); total == 0 {
		t.Fatal("no service was created, so the two assertions above prove nothing")
	}
}

// No url asset, whatever the file carries. 4.3 makes it a rule about producers
// and this is a producer.
func TestAnImportCreatesNoURLAsset(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	scan := realScan(t)
	if count := scan.Counts[bbot.TypeURL]; count == nil || count.Seen == 0 {
		t.Fatal("the fixture carries no URL event, so this test measures nothing")
	}
	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, scan); err != nil {
		t.Fatalf("import: %v", err)
	}

	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1 AND kind = 'url'`, h.program); n != 0 {
		t.Errorf("%d url assets, and no producer creates one", n)
	}
}

// Importing the same file twice creates nothing the second time. The unique
// constraint is what makes that true, and this asserts it rather than a cache.
func TestTheSameFileTwiceCreatesNothingTheSecondTime(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	first, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Per asset, not one date for all of them: each row back-dates to its own
	// event's timestamp and those differ across the file, so comparing every
	// row against a single value measures the fixture rather than the write.
	before := h.firstSeen(t)

	second, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	after := h.firstSeen(t)

	if len(before) != len(after) {
		t.Errorf("assets went from %d to %d on a repeat of the same file", len(before), len(after))
	}
	if second.Assets.Created != 0 {
		t.Errorf("the second import reports %d created", second.Assets.Created)
	}
	if second.Assets.Existing != first.Assets.Created+first.Assets.Existing {
		t.Errorf("the second import saw %d existing against %d written the first time",
			second.Assets.Existing, first.Assets.Created+first.Assets.Existing)
	}

	// And no date moved. Counting rows would pass on an upsert that rewrote
	// every first_seen, which is the half of idempotence that does not show up
	// as a row and is the one a cursor walking the inventory would notice.
	for key, was := range before {
		if now, ok := after[key]; !ok {
			t.Errorf("%s disappeared on the repeat", key)
		} else if !was.Equal(now) {
			t.Errorf("%s moved its first_seen from %s to %s", key, was, now)
		}
	}
}

// first_seen is the moment of the import, like every other act that writes an
// asset, and the scan's own date is in the lineage where it belongs.
//
// An earlier version of this phase back-dated the row to the event's timestamp.
// It read well and it fought two mechanisms that both take first_seen to mean
// "when this platform knew": the feed walks it as a cursor, so nothing an import
// created was ever emitted, and the candidate budget measures from it, so a
// three week old file arrived already expired. 7.6 carries the reasoning.
func TestTheRowIsDatedByTheImportAndTheScanDateIsInTheLineage(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}

	scan := stream(t, `{"type":"DNS_NAME","id":"1","data":"old.acme.test","timestamp":1785346188.0,"module":"crt_db"}`)
	if _, err := h.dated(c).Import(context.Background(), h.queries, h.importRun(), set, scan); err != nil {
		t.Fatalf("import: %v", err)
	}

	var first time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT first_seen FROM asset WHERE program_id = $1 AND key = $2`,
		h.program, "old.acme.test").Scan(&first); err != nil {
		t.Fatalf("read first_seen: %v", err)
	}
	if !first.UTC().Equal(c.now) {
		t.Errorf("first_seen = %s, want the moment of the import %s", first.UTC(), c.now)
	}

	// And the scan's date is not lost, which is the half that makes the
	// decision above affordable.
	step := h.lineageOf(t, "old.acme.test")
	want := time.Unix(1785346188, 0).UTC().Format(time.RFC3339)
	if step["at"] != want {
		t.Errorf("lineage at = %v, want the event's own date %s", step["at"], want)
	}
}

// An imported candidate gets the whole budget, which is what the back-dating
// took away. The discriminating case is a file older than the budget itself.
func TestAnImportedCandidateIsNotAlreadyExpired(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	// Three weeks old, against a fourteen day budget.
	old := c.now.Add(-21 * 24 * time.Hour).Unix()
	scan := stream(t, fmt.Sprintf(
		`{"type":"DNS_NAME","id":"1","data":"stale.acme.test","timestamp":%d.0}`, old))
	if _, err := ing.Import(context.Background(), h.queries, h.importRun(), set, scan); err != nil {
		t.Fatalf("import: %v", err)
	}

	// One resolve that finds nothing, which is the normal fate of most names in
	// an imported file.
	run := h.run()
	run.Scope = "resolve"
	run.Targets = map[string]struct{}{"stale.acme.test": {}}
	c.now = c.now.Add(time.Minute)
	if _, err := ing.Report(context.Background(), h.queries, run, set,
		deadHost("stale.acme.test", "nxdomain")); err != nil {
		t.Fatalf("ingest the resolve report: %v", err)
	}

	if got := h.lifecycleOf(t, "stale.acme.test"); got == "archived" {
		t.Error("given up on after one check, with none of the fourteen day budget spent")
	}
}

// An import does not resurrect what this system concluded was gone. The file is
// dated by whoever ran the scan and can be older than the archival it would
// undo, which is what separates it from the assets form and from a certificate.
func TestAnImportDoesNotReviveAnArchivedAsset(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))

	scan := stream(t, `{"type":"DNS_NAME","id":"1","data":"gone.acme.test","timestamp":1785346188.0}`)
	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, scan); err != nil {
		t.Fatalf("first import: %v", err)
	}
	exec(t, h.pool,
		`UPDATE asset_current SET lifecycle = 'archived', next_resolve_at = NULL
		  WHERE program_id = $1 AND key = $2`, h.program, "gone.acme.test")

	second, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, scan)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	// The answer says what will be looked at, so it must not count this one.
	if second.Assets.Scheduled != 0 {
		t.Errorf("scheduled = %d, and the only asset in the file is archived",
			second.Assets.Scheduled)
	}

	var lifecycle string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lifecycle FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, "gone.acme.test").Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	if lifecycle != "archived" {
		t.Errorf("lifecycle = %q, want it left archived", lifecycle)
	}

	// And it carries no due date, which is the half that turns "does not
	// revive" into something more than a label. An archived row has no dates,
	// so the upsert's COALESCE would hand it the import's, and every selection
	// filters archived out again: nothing would ever claim it while the queue
	// counted it as due forever.
	if due := h.nullableDue(t, "next_resolve_at", "gone.acme.test"); due != nil {
		t.Errorf("an archived asset is due at %s, and no selection will ever take it", due)
	}

	// The discriminating half: the assets form on the same name does revive,
	// so this is a property of the import and not of the write underneath it.
	if _, err := h.ing.Enter(context.Background(), h.queries, h.run(), set,
		[]string{"gone.acme.test"}); err != nil {
		t.Fatalf("enter by hand: %v", err)
	}
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lifecycle FROM asset_current WHERE program_id = $1 AND key = $2`,
		h.program, "gone.acme.test").Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	if lifecycle == "archived" {
		t.Error("the assets form did not revive either, so the test above measures nothing")
	}
}

// Lineage is where what the tool measured goes, and it is the answer to "why is
// this here" six months later.
func TestTheLineageCarriesWhatTheToolSaid(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t)); err != nil {
		t.Fatalf("import: %v", err)
	}

	step := h.lineageOf(t, "acme-corp.test")
	if step["step"] != "imported" || step["tool"] != "bbot" {
		t.Errorf("lineage = %v, want the import naming itself", step)
	}
	if step["module"] == nil || step["context"] == nil {
		t.Errorf("lineage = %v, want the module and the sentence it wrote", step)
	}
	if source := h.sourceOf(t, "acme-corp.test"); source != ingest.SourceBBOT {
		t.Errorf("discovery_source = %q, want %q", source, ingest.SourceBBOT)
	}

	// The technologies the tool reported live here and nowhere else: they are
	// not measured state and no column of asset_current carries them.
	found := false
	for _, host := range realScan(t).Hosts {
		if len(host.Technologies) == 0 {
			continue
		}
		found = true
		step := h.lineageOf(t, host.Name)
		if step["technologies"] == nil {
			t.Errorf("%s reported technologies and its lineage carries none", host.Name)
		}
		if n := h.count(t,
			`SELECT count(*) FROM asset_current WHERE program_id = $1 AND key = $2
			   AND technologies IS NOT NULL AND cardinality(technologies) > 0`,
			h.program, host.Name); n != 0 {
			t.Errorf("%s has technologies on asset_current, which only a fingerprint may write", host.Name)
		}
	}
	if !found {
		t.Fatal("no host in the fixture reported a technology, so half of this test measures nothing")
	}
}

// discovery_source answers "why is this here" with the first appearance, so an
// import that names an asset a scanner already found does not rewrite history.
func TestAnImportDoesNotStealTheLineageOfAnAssetSomethingElseFound(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))

	if _, err := h.ing.Enter(context.Background(), h.queries, h.run(), set,
		[]string{"shared.acme.test"}); err != nil {
		t.Fatalf("enter by hand: %v", err)
	}

	scan := stream(t, `{"type":"DNS_NAME","id":"1","data":"shared.acme.test","timestamp":1785346188.0}`)
	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, scan); err != nil {
		t.Fatalf("import: %v", err)
	}

	if source := h.sourceOf(t, "shared.acme.test"); source != ingest.SourceManual {
		t.Errorf("discovery_source = %q, want the first appearance to stand", source)
	}
}

// The answer says where every line went. A count of assets on its own reads as
// the whole answer and never is.
func TestTheAnswerAccountsForEveryEventType(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	imported, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, realScan(t))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	for _, want := range []string{
		bbot.TypeScan, bbot.TypeDNSName, bbot.TypeOpenTCPPort,
		bbot.TypeURL, bbot.TypeProtocol, bbot.TypeTechnology, bbot.TypeASN,
	} {
		count := imported.Events[want]
		if count == nil || count.Seen == 0 {
			t.Errorf("%s is in the file and unaccounted for in the answer", want)
			continue
		}
		if count.Hosts == 0 && count.Services == 0 && count.Note == "" {
			t.Errorf("%s produced nothing and did not say why", want)
		}
	}

	if imported.Scan.Name != "reference" {
		t.Errorf("the answer names the scan %q", imported.Scan.Name)
	}
	if imported.Assets.Hosts+imported.Assets.Services != imported.Assets.Created+imported.Assets.Existing {
		t.Errorf("hosts and services (%d) do not sum to created and existing (%d)",
			imported.Assets.Hosts+imported.Assets.Services,
			imported.Assets.Created+imported.Assets.Existing)
	}
}

// A name the platform cannot turn into an identity is named individually, not
// swallowed by a total. A list where three entries went nowhere is the failure
// mode this endpoint has.
func TestAnEntryThatIsNotAnIdentityIsNamed(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))

	scan := stream(t,
		`{"type":"DNS_NAME","id":"1","data":"good.acme.test","timestamp":1785346188.0}`,
		`{"type":"DNS_NAME","id":"2","data":"not a host","timestamp":1785346188.0}`)

	imported, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, scan)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(imported.Refused) != 1 {
		t.Fatalf("refused = %v, want the one entry that was not a name", imported.Refused)
	}
	if !strings.Contains(imported.Refused[0].Entry, "not a host") {
		t.Errorf("the refusal does not name the entry: %v", imported.Refused[0])
	}
	if imported.Assets.Created != 1 {
		t.Errorf("created = %d, want the good entry to have landed anyway", imported.Assets.Created)
	}
}

// The milestone's first assertion, and the one that needs two files rather than
// one. subdomains.txt is written by the tool itself, independently of the event
// stream, so comparing the created fqdn assets against it checks the decoder
// against something that is not the decoder. A silently dropped name is exactly
// what a test reading only the stream would miss.
func TestTheReferenceScanCreatesEveryNameTheToolListed(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme-corp.test"))

	file, err := os.Open("../bbot/testdata/reference.ndjson")
	if err != nil {
		t.Fatalf("open the reference scan: %v", err)
	}
	defer func() { _ = file.Close() }()
	scan, err := bbot.Parse(file)
	if err != nil {
		t.Fatalf("parse the reference scan: %v", err)
	}

	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, scan); err != nil {
		t.Fatalf("import: %v", err)
	}

	listed, err := os.ReadFile("../bbot/testdata/subdomains.txt")
	if err != nil {
		t.Fatalf("read the tool's own list: %v", err)
	}
	want := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(listed)), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			want[name] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("the tool's list is empty, so this test measures nothing")
	}

	rows, err := h.pool.Query(context.Background(),
		`SELECT key FROM asset WHERE program_id = $1 AND kind = 'fqdn' AND scope_status = 'in_scope'`,
		h.program)
	if err != nil {
		t.Fatalf("read the created names: %v", err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[key] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for name := range want {
		if !got[name] {
			t.Errorf("%s is in the tool's own list and was not created", name)
		}
	}
	// The other direction, because a decoder that created every name in the
	// file plus a handful of parsing artefacts would pass the loop above.
	for name := range got {
		if !want[name] {
			t.Errorf("%s was created in scope and the tool never listed it", name)
		}
	}
}

// The second half of "resolve, then full once it answers", and the assertion
// that this phase added no promotion of its own. An imported name that resolves
// earns the expensive rung through the same reschedule path a certificate
// candidate does. Without it a name is created with no full date, checked only
// by resolve runs which leave that date alone, and swept for ports never.
func TestAnImportedNameThatAnswersEarnsTheExpensiveRung(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))
	c := &clock{now: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)}
	ing := h.dated(c)

	const name = "live.acme.test"
	scan := stream(t, fmt.Sprintf(
		`{"type":"DNS_NAME","id":"1","data":%q,"timestamp":1785346188.0}`, name))
	if _, err := ing.Import(context.Background(), h.queries, h.importRun(), set, scan); err != nil {
		t.Fatalf("import: %v", err)
	}
	if full := h.nullableDue(t, "next_full_at", name); full != nil {
		t.Fatalf("the import already carried a full date at %s", full)
	}

	// A resolve run, which is the scope that leaves the full date alone
	// everywhere else. If a date appears it is the promotion and nothing else.
	resolve := h.run()
	resolve.Scope = "resolve"
	resolve.Targets = map[string]struct{}{name: {}}

	c.now = c.now.Add(time.Minute)
	if _, err := ing.Report(context.Background(), h.queries, resolve, set, liveHost(name)); err != nil {
		t.Fatalf("ingest the resolve report: %v", err)
	}

	if got := h.lifecycleOf(t, name); got != "active" {
		t.Fatalf("the imported name answered and is %q", got)
	}
	full := h.nullableDue(t, "next_full_at", name)
	if full == nil {
		t.Fatal("an imported name that answered carries no full due date, so nothing will ever " +
			"sweep its ports and whatever it exposes is invisible")
	}
	if full.Before(c.now) || full.After(c.now.Add(time.Hour)) {
		t.Errorf("the promoted full date is %s, and the name answered at %s", full, c.now)
	}
}

// A service whose host this platform cannot name is refused with it, rather
// than written as a service with no parent.
//
// hostKey and normalize.Service disagree on exactly one shape: a single label
// host, which Service accepts and FQDN does not. The decoder creates a host for
// every service it emits precisely so a service always has a parent, and that
// guarantee only holds if the write refuses the pair together.
func TestAServiceWhoseHostIsNotAnIdentityIsRefusedWithIt(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))

	scan := stream(t,
		`{"type":"OPEN_TCP_PORT","id":"1","host":"intranet","port":8443,"timestamp":1785346100.0}`,
		`{"type":"OPEN_TCP_PORT","id":"2","host":"ok.acme.test","port":443,"timestamp":1785346100.0}`)

	imported, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, scan)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if n := h.count(t,
		`SELECT count(*) FROM asset WHERE program_id = $1 AND kind = 'service'
		   AND parent_asset_id IS NULL`, h.program); n != 0 {
		t.Errorf("%d services have no host, and their scheduling is carried by nothing", n)
	}
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1 AND key LIKE 'intranet%'`,
		h.program); n != 0 {
		t.Errorf("%d assets were written for a name that is not an identity", n)
	}
	// Two refusals, the host and the service on it, each named.
	if len(imported.Refused) != 2 {
		t.Errorf("refused = %+v, want the host and its service", imported.Refused)
	}
	// The positive control: the good pair still landed, so the assertions above
	// are not passing on an import that refused everything.
	if n := h.count(t, `SELECT count(*) FROM asset WHERE program_id = $1`, h.program); n != 2 {
		t.Errorf("%d assets, want the good host and its service", n)
	}
}

// The answer names entries one by one and the list is bounded, so a file that
// is wrong in bulk does not produce a body bigger than the file. What the bound
// held back is counted rather than dropped.
func TestTheRefusalListInTheAnswerIsBounded(t *testing.T) {
	h := newHarness(t)
	set := h.scope(t, include("acme.test"))

	// Single label names, which normalize refuses as identities.
	lines := make([]string, 0, 300)
	for i := range 300 {
		lines = append(lines, fmt.Sprintf(
			`{"type":"DNS_NAME","id":"%d","data":"host%d","timestamp":1785346100.0}`, i, i))
	}

	imported, err := h.ing.Import(context.Background(), h.queries, h.importRun(), set, stream(t, lines...))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if len(imported.Refused) > 101 {
		t.Errorf("refused carries %d entries for a file of 300 bad names", len(imported.Refused))
	}
	last := imported.Refused[len(imported.Refused)-1]
	if !strings.Contains(last.Entry, "further entries") {
		t.Errorf("the list stops without saying so: %+v", last)
	}
	if imported.Assets.Created != 0 {
		t.Errorf("created = %d, want none of them written", imported.Assets.Created)
	}
}

// A rule that brings an abandoned asset back into scope does not by itself
// decide to chase it again, and must not leave a date nothing can claim. The
// same fault as the import's, in the statement the reclassification uses.
func TestReclassifyingAnArchivedAssetIntoScopeSchedulesNothing(t *testing.T) {
	h := newHarness(t)

	// In scope first, so the row exists, then archived, then out of scope.
	inScope := h.scope(t, include("acme.test"))
	scan := stream(t, `{"type":"DNS_NAME","id":"1","data":"gone.acme.test","timestamp":1785346100.0}`)
	if _, err := h.ing.Import(context.Background(), h.queries, h.importRun(), inScope, scan); err != nil {
		t.Fatalf("import: %v", err)
	}
	exec(t, h.pool, `UPDATE asset_current SET lifecycle='archived', next_resolve_at=NULL, next_full_at=NULL
	                  WHERE program_id=$1 AND key=$2`, h.program, "gone.acme.test")
	exec(t, h.pool, `UPDATE asset SET scope_status='unknown' WHERE program_id=$1 AND key=$2`,
		h.program, "gone.acme.test")
	exec(t, h.pool, `UPDATE asset_current SET scope_status='unknown' WHERE program_id=$1 AND key=$2`,
		h.program, "gone.acme.test")

	// And now a rule brings it back, with due dates, which is what the write
	// path passes for every asset moving into scope.
	at := time.Now()
	if _, err := h.ing.Reclassify(context.Background(), h.queries, h.program, inScope,
		ingest.Schedule{Resolve: &at, Full: &at}); err != nil {
		t.Fatalf("reclassify: %v", err)
	}

	var status, life string
	var due *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT scope_status, lifecycle, next_resolve_at FROM asset_current
		  WHERE program_id=$1 AND key=$2`, h.program, "gone.acme.test").Scan(&status, &life, &due); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if status != "in_scope" {
		t.Fatalf("scope_status = %q, so the reclassification did not run and this proves nothing", status)
	}
	if due != nil {
		t.Errorf("an archived asset is due at %s, and every selection filters it out", due)
	}
}
