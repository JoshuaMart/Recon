package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/enrich"
	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/signals"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxDerivedPorts bounds what one host may imply. A host answering on a quarter
// of the curated list is a tarpit, a device accepting everything or an edge,
// not twenty-five services. The observation keeps the full port list either way.
const maxDerivedPorts = 24

// ErrOutOfScope is returned when a report names a host the run was not given.
// It is rejected, not ignored, so a scanner cannot choose its own perimeter.
var ErrOutOfScope = errors.New("host is outside the run's frozen target list")

// Run is what the caller knows about the execution that produced a report.
type Run struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	ProgramID uuid.UUID
	Kind      string
	// Scope is the rung the run reached; it decides which due dates the report moves.
	Scope string
	// Targets is the frozen list keyed by canonical host. Nil on a discovery run.
	Targets map[string]struct{}
	// Source is the origin an entered asset records. Empty means the assets form.
	Source string
	// Certificate is what revealed the names, for the Certificate Transparency
	// source. Nil elsewhere.
	Certificate *Certificate
	// Revive brings an archived asset back. True for acts this platform dates
	// itself (typing, a log publishing), false for an import, whose file is dated
	// by whoever ran the scan and may predate the archival.
	Revive bool
	// Due is when a new asset becomes due. The arithmetic is in Go to be testable.
	Due Schedule
	// Grace is what this programme's first run may keep quiet. The caller reads it
	// with the run and it is frozen for the batch, so a notifier deciding at drain
	// time cannot send a first run's flood late.
	Grace notify.Grace
}

// Schedule is the first due date of a new asset.
type Schedule struct {
	Resolve *time.Time
	Full    *time.Time
}

// Summary is what one report changed. Fields are tagged because it is stored in
// run.summary and read back by the queue view: the names are a wire contract,
// and a rename would break the console without failing to compile.
type Summary struct {
	Hosts        int `json:"hosts"`
	Assets       int `json:"assets"`
	Created      int `json:"created"`
	Observations int `json:"observations"`
	Deduplicated int `json:"deduplicated"`
	Rejected     int `json:"rejected"`
	Skipped      int `json:"skipped"`
	// Late marks a report that arrived after the run's deadline. The data is still
	// valid (the run may have been re-dispatched), so it is recorded and
	// deduplication merges the two.
	Late    bool `json:"late"`
	Derived int  `json:"derived"`
	// Takeovers counts the findings this report carried.
	Takeovers int `json:"takeovers"`
	// Sources is the scanner's per-source accounting. A keyed source without its
	// credential disables itself silently: the run finishes correctly and finds
	// less, which nothing else shows.
	Sources []SourceReport `json:"sources,omitempty"`
	// Notifications is accumulated and written in one statement at the end of the
	// transaction. Excluded from the summary: it is a batch in flight, not a fact
	// about the run.
	Notifications []notify.Event `json:"-"`
	// grace is read once per report and frozen with the payload.
	grace notify.Grace
	// Suppressed counts events held back as a first run's flood.
	Suppressed int            `json:"suppressed"`
	Unknown    map[string]int `json:"unknown,omitempty"`
}

// SourceReport is one enumeration source and how it went.
type SourceReport struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Found  int    `json:"found"`
	Error  string `json:"error,omitempty"`
}

// Ingestor writes reports into the inventory.
type Ingestor struct {
	enricher enrich.Enricher
	cadence  lifecycle.Cadence
	// random draws the jitter every delay is widened by, so assets written by one
	// run do not share a due date forever. A field so tests assert a date.
	random func() float64
	now    func() time.Time
	log    *slog.Logger
}

// Option adjusts an ingestor. The clock and the random draw are inputs of a due
// date and must be injectable; asserting a range would pass a constant.
type Option func(*Ingestor)

// WithClock replaces the instant every observation is stamped with.
func WithClock(now func() time.Time) Option {
	return func(i *Ingestor) {
		if now != nil {
			i.now = now
		}
	}
}

// WithJitter replaces the draw every delay is widened by.
func WithJitter(random func() float64) Option {
	return func(i *Ingestor) {
		if random != nil {
			i.random = random
		}
	}
}

// New builds an ingestor.
func New(enricher enrich.Enricher, cadence lifecycle.Cadence, log *slog.Logger, opts ...Option) *Ingestor {
	if enricher == nil {
		enricher = enrich.Nothing()
	}
	if cadence.Resolve <= 0 {
		cadence = lifecycle.DefaultCadence()
	}
	ingestor := &Ingestor{
		enricher: enricher,
		cadence:  cadence,
		random:   rand.Float64,
		now:      time.Now,
		log:      log,
	}
	for _, opt := range opts {
		opt(ingestor)
	}
	return ingestor
}

// Report writes a whole report inside one transaction.
//
// Scope, outcome, implied assets and resulting state are decided here, not
// believed: a scanner that lied about any of them is reclassified, requalified
// and bounded on arrival.
func (i *Ingestor) Report(ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, report Report) (Summary, error) {
	summary := Summary{Unknown: map[string]int{}}

	summary.grace = run.Grace

	for _, host := range report.Hosts {
		summary.Hosts++

		key, err := hostKey(host.Host)
		if err != nil {
			// A source can return junk. One unusable name does not refuse the report, but
			// it is counted.
			summary.Skipped++
			i.log.WarnContext(ctx, "host skipped", "host", host.Host, "error", err)
			continue
		}

		if run.Targets != nil {
			if _, asked := run.Targets[key.Value]; !asked {
				summary.Rejected++
				i.log.WarnContext(ctx, "host outside the frozen target list",
					"run", run.ID, "host", key.Value)
				continue
			}
		}

		st, err := i.writeAsset(ctx, q, run, set, key, host, nil)
		if err != nil {
			return summary, err
		}
		summary.Assets++
		if st.created {
			summary.Created++
		}

		// Being behind an edge is structural and seen identically by every observer,
		// so it is decided once per host and carried to derived services.
		edge := i.edge(host)

		answered, err := i.writeHostObservations(ctx, q, run, report, st, host, edge, &summary)
		if err != nil {
			return summary, err
		}
		if err := i.writeServices(ctx, q, run, set, report, key, st, host, edge, &summary); err != nil {
			return summary, err
		}

		// A host the report did not answer for keeps its due date and is selected again.
		// Silence is not a measurement; treating it as one archives live assets on a
		// truncated run.
		if !answered {
			continue
		}
		if err := i.reschedule(ctx, q, run, st); err != nil {
			return summary, err
		}
	}

	summary.Sources = accounting(report.Sources)

	// A declared path earns its render once its service has answered. One
	// statement for the report, since it reads state the observations above wrote.
	if err := i.writeEvents(ctx, q, run, &summary); err != nil {
		return summary, err
	}

	if err := q.ScheduleDeclaredURLs(ctx, sqlcgen.ScheduleDeclaredURLsParams{
		ProgramID: uuidTo(run.ProgramID),
		At:        stamp(i.now()),
		Priority:  lifecycle.PriorityBaseline,
	}); err != nil {
		return summary, fmt.Errorf("schedule declared urls: %w", err)
	}

	return summary, nil
}

// hostKey turns a reported name into an identity. An address literal is an
// address asset; the report does not say which it sent.
func hostKey(raw string) (normalize.Key, error) {
	if key, err := normalize.IP(raw); err == nil {
		return key, nil
	}
	return normalize.FQDN(raw)
}

// edge decides whether a host sits behind a CDN, and which one.
func (i *Ingestor) edge(host Host) promoted {
	var provider string
	for _, cdn := range host.CDN {
		if cdn.Name != "" {
			provider = cdn.Name
			break
		}
	}

	var operator string
	if addrs := addresses(host.Addresses); len(addrs) > 0 {
		operator = i.enricher.Lookup(addrs[0]).ASNOrg
	}

	// A pass with nothing to look at has no opinion, which differs from "not fronted": a
	// timed-out resolution carries no address, CNAME or provider, and writing false would clear
	// the flag on a fronted asset whenever the name failed to answer. A resolved name was looked
	// at whatever enrichment could add: the scanner's determination and the terminal CNAME are
	// real evidence, the operator the third source.
	if provider == "" && len(host.CNAME) == 0 && len(host.Addresses) == 0 {
		return promoted{}
	}

	fronted, name := signals.CDN(provider, host.CNAME, operator)
	return promoted{IsCDN: &fronted, CDNProvider: text(name)}
}

func (i *Ingestor) writeAsset(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set,
	key normalize.Key, host Host, parent *uuid.UUID,
) (*state, error) {
	target := scope.Target{Key: key, Addresses: addresses(host.Addresses)}
	status := set.Classify(target)

	// A derived service was implied by an open port, not returned by a source.
	// Copying the host's step would attribute it to whichever source returned the
	// name, so "what did this source find" would count every service.
	step := derived(run, key)
	if key.Kind != normalize.KindService {
		step = lineage(run, host)
	}
	path, err := json.Marshal([]any{step})
	if err != nil {
		return nil, err
	}

	params := sqlcgen.UpsertAssetAndProjectionParams{
		AssetID:         uuidTo(uuid.New()),
		OrgID:           uuidTo(run.OrgID),
		ProgramID:       uuidTo(run.ProgramID),
		Kind:            string(key.Kind),
		Key:             key.Value,
		Host:            text(key.Host),
		DiscoverySource: discoverySource(run, host),
		ScopeStatus:     string(status),
		SeenAt:          stamp(i.now()),
	}
	if key.Port != 0 {
		params.Port = portPtr(key.Port)
	}
	if key.Scheme != "" {
		params.Scheme = text(key.Scheme)
	}
	if parent != nil {
		params.ParentAssetID = uuidTo(*parent)
	}
	if len(path) > 0 {
		params.DiscoveryPath = path
	}
	i.enrichInto(&params, target.Addresses)
	// Only in-scope hosts and addresses are scheduled. A service is observed
	// through its host's run, so a resolve date on one would sit in a queue nothing
	// dispatches from.
	if status == scope.InScope && (key.Kind == normalize.KindFQDN || key.Kind == normalize.KindIP) {
		params.NextResolveAt = stampPtr(run.Due.Resolve)
		params.NextFullAt = stampPtr(run.Due.Full)
	}

	row, err := q.UpsertAssetAndProjection(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("upsert asset %s: %w", key.Value, err)
	}

	st, err := newState(row, key.Kind, key.Port)
	if err != nil {
		return nil, err
	}
	st.key = key.Value
	st.lineage = path
	// On a first appearance there is no previous source; the one being written is
	// the answer.
	if st.source == "" {
		st.source = params.DiscoverySource
	}
	return st, nil
}

// enrichInto turns the address a run connected to into an operator and a place.
//
// It reads the submitted payload, not the normalized one: a round-robin name
// answers a different address each pass, so normalization drops the field, and
// keeping it would make the payload differ on every run for an untouched target.
// This is the only such exception in the write path.
func (i *Ingestor) enrichInto(params *sqlcgen.UpsertAssetAndProjectionParams, addrs []netip.Addr) {
	if len(addrs) == 0 {
		return
	}
	addr := addrs[0]
	params.Ip = addr

	found := i.enricher.Lookup(addr)
	if found.Empty() {
		return
	}
	if found.ASN != 0 {
		params.Asn = asnPtr(found.ASN)
	}
	params.AsnOrg = text(found.ASNOrg)
	params.Country = text(found.Country)
	params.Region = text(found.Region)
	params.City = text(found.City)
}

// writeHostObservations writes what a run learned about a name and reports
// whether the run answered for the host at all.
func (i *Ingestor) writeHostObservations(
	ctx context.Context, q *sqlcgen.Queries, run Run, report Report,
	st *state, host Host, edge promoted, summary *Summary,
) (bool, error) {
	// A host the run never reached produces nothing. A verdict for a name never
	// queried is worse than the gap, and archives live assets on a truncated run.
	if host.Status == StatusDiscovered {
		return false, nil
	}

	dns := map[string]any{"status": host.Status}
	if host.Reason != "" {
		dns["reason"] = host.Reason
	}
	if len(host.Addresses) > 0 {
		dns["addresses"] = anySlice(host.Addresses)
	}
	if len(host.CNAME) > 0 {
		dns["cname"] = anySlice(host.CNAME)
	}

	// A name pointing at a name that no longer exists. The dangling pointer and its
	// proof arrive in one query: a recursive resolver follows the chain and returns
	// nxdomain with the CNAME still in the answer.
	takeover := signals.Dangling(host.Status, host.Reason, host.CNAME)
	if takeover != nil {
		dns["takeover_candidate"] = takeover.Map()
		summary.Takeovers++
	}

	if err := i.apply(ctx, q, run, report, st, observation{
		layer:        normalize.LayerDNS,
		outcome:      dnsOutcome(host),
		data:         dns,
		promoted:     edge,
		takeover:     takeover,
		takeoverKind: signals.KindOrphanCNAME,
	}, summary); err != nil {
		return true, err
	}

	outcome := tcpOutcome(host)
	if outcome == "" {
		return true, nil
	}

	tcp := map[string]any{"open_ports": ports(host.Ports)}
	if len(host.Addresses) > 0 {
		tcp["addresses"] = anySlice(host.Addresses)
	}
	if len(host.CDN) > 0 {
		tcp["cdn"] = jsonAny(host.CDN)
	}
	if host.Scan != nil {
		tcp["scan"] = jsonAny(host.Scan)
	}
	if err := i.apply(ctx, q, run, report, st, observation{
		layer:    normalize.LayerTCP,
		outcome:  outcome,
		data:     tcp,
		promoted: edge,
		// No takeover kind: a finding is cleared only by the layer that could produce
		// it. Claiming the orphan CNAME kind here let a tcp observation delete the
		// finding the dns observation had just written.
	}, summary); err != nil {
		return true, err
	}

	return true, nil
}

// writeServices turns every open port into an asset. Without it the port scan
// buys nothing: no due date, no HTTP probe, no renderer, so a forgotten
// application on an unusual port could not enter the inventory.
func (i *Ingestor) writeServices(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, report Report,
	hostKey normalize.Key, host *state, reported Host, edge promoted, summary *Summary,
) error {
	// Only a host derives services; deriving from a service would recreate itself.
	if hostKey.Kind != normalize.KindFQDN && hostKey.Kind != normalize.KindIP {
		return nil
	}
	if len(reported.Ports) > maxDerivedPorts {
		i.log.WarnContext(ctx, "too many open ports to derive services",
			"host", hostKey.Value, "open", len(reported.Ports), "bound", maxDerivedPorts)
		return nil
	}

	for _, port := range reported.Ports {
		// The derived key's host is the observed asset's host, never a payload field,
		// so a scanner given one target cannot create services on another.
		key, err := normalize.Service(hostKey.Host, port.Port, port.Protocol)
		if err != nil {
			summary.Skipped++
			continue
		}

		// The scheme is what answered, and it is where a render is later pointed. The
		// port number would be a guess for TLS on unusual ports, and an unlowered
		// "HTTPS" would point a browser at a different service on the same host.
		if port.HTTP != nil {
			if scheme, err := normalize.Scheme(port.HTTP.Scheme); err == nil {
				key.Scheme = scheme
			}
		}
		service, err := i.writeAsset(ctx, q, run, set, key, reported, &host.id)
		if err != nil {
			return err
		}
		summary.Assets++
		summary.Derived++
		if service.created {
			summary.Created++
		}

		// A service earns its first render on transport reachability alone, not the outcome: an
		// origin error behind a CDN counts as death yet is a page worth a baseline. It does read the
		// instrument: Chrome answers ERR_UNSAFE_PORT on its restricted list, a certain failure and
		// not a measurement. UDP is excluded since a browser speaks none and the render queue
		// carries no protocol to notice later.
		if key.Kind == normalize.KindService && port.Protocol != "udp" && fingerprint.Renderable(port.Port) {
			if err := i.earnBaseline(ctx, q, service); err != nil {
				return err
			}
		}

		// A derived service stays a candidate until something addresses the service
		// itself. The port scan is about the host; reading it as about the service
		// would report every open port as a verified application.
		if port.HTTP == nil {
			continue
		}
		if err := i.writeService(ctx, q, run, report, service, port, edge, summary); err != nil {
			return err
		}
	}

	return nil
}

// writeService records what the HTTP probe got out of one port.
func (i *Ingestor) writeService(
	ctx context.Context, q *sqlcgen.Queries, run Run, report Report,
	service *state, port Port, edge promoted, summary *Summary,
) error {
	payload, err := toMap(port.HTTP)
	if err != nil {
		return err
	}

	verdict := signals.Read(signals.Response{
		StatusCode: port.HTTP.StatusCode,
		Server:     port.HTTP.Server,
		Title:      port.HTTP.Title,
		Tech:       port.HTTP.Tech,
		Fronted:    edge.IsCDN != nil && *edge.IsCDN,
		Provider:   derefString(edge.CDNProvider),
	})

	// A response is not liveness: behind a CDN the edge always answers, so a dead
	// origin would stay active forever. Death there is only readable in the
	// semantics of the response.
	outcome := OutcomeOK
	if verdict.Dead != "" {
		outcome = OutcomeFail
		payload["origin_dead"] = verdict.Dead
	}

	// Orthogonal to the outcome: a 403 with a mitigation signature is a target that
	// answered and is there, and a probe that learned nothing.
	usable := verdict.Usable()
	if verdict.Challenge != "" {
		payload["waf_detected"] = true
		payload["waf_source"] = "http"
		if verdict.Vendor != "" {
			payload["waf_vendor"] = verdict.Vendor
		}
	}
	if edge.IsCDN != nil {
		payload["is_cdn"] = *edge.IsCDN
	}
	if edge.CDNProvider != nil {
		payload["cdn_provider"] = *edge.CDNProvider
	}

	takeover := signals.Unclaimed(port.HTTP.URL, verdict)
	if takeover != nil {
		payload["takeover_candidate"] = takeover.Map()
		summary.Takeovers++
	}

	columns := edge
	columns.StatusCode = portPtr(port.HTTP.StatusCode)
	columns.FinalURL = text(port.HTTP.FinalURL)
	columns.Title = text(port.HTTP.Title)
	columns.Server = text(port.HTTP.Server)
	columns.Technologies = port.HTTP.Tech
	if verdict.Challenge != "" {
		detected := true
		columns.WAFDetected = &detected
		columns.WAFVendor = text(verdict.Vendor)
	}

	return i.apply(ctx, q, run, report, service, observation{
		layer:        normalize.LayerHTTP,
		outcome:      outcome,
		data:         payload,
		promote:      true,
		promoted:     columns,
		usable:       &usable,
		takeover:     takeover,
		takeoverKind: signals.KindUnclaimedService,
	}, summary)
}

// earnBaseline gives a service its first render date, only where there is
// none, so a queued service keeps its place.
//
// It is due when earned and does not inherit the discovery jitter: that spread
// is for the first probe of a fresh asset, and this line is created later.
func (i *Ingestor) earnBaseline(ctx context.Context, q *sqlcgen.Queries, service *state) error {
	if err := q.EarnBaseline(ctx, sqlcgen.EarnBaselineParams{
		AssetID:  uuidTo(service.id),
		At:       stamp(i.now()),
		Priority: lifecycle.PriorityBaseline,
	}); err != nil {
		return fmt.Errorf("earn a baseline for %s: %w", service.id, err)
	}
	return nil
}

// tcpOutcome reads what the port sweep proved; empty when it proved nothing.
//
// A report listing only open ports cannot conclude a death: an empty list means
// both "nothing was open" and "nothing was tried". Until reports carry counts,
// this layer writes nothing rather than an unsupported verdict.
func tcpOutcome(host Host) string {
	if len(host.Ports) > 0 {
		return OutcomeOK
	}
	if host.Scan == nil || !host.Scan.Accounted() {
		return ""
	}
	// The host answers and nothing listens. Filtered is indistinguishable from
	// banned and may hide a service, and unknown was never measured; counts that
	// contradict the port list were already handled above and must not become a
	// death either.
	if host.Scan.Open == 0 && host.Scan.Refused > 0 &&
		host.Scan.Filtered == 0 && host.Scan.Unknown == 0 {
		return OutcomeFail
	}
	return OutcomeError
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Outcomes describe what was learned about the target, not whether the probe
// ran.
const (
	OutcomeOK    = "ok"
	OutcomeFail  = "fail"
	OutcomeError = "error"
)

// dnsOutcome reads what the resolution proved.
func dnsOutcome(host Host) string {
	switch host.Status {
	case StatusLive, StatusWildcard:
		return OutcomeOK
	case StatusDead:
		switch host.Reason {
		case ReasonNXDomain:
			// The only clean death signal.
			return OutcomeFail
		case ReasonNoAnswer:
			// A name with no address (MX-only, TXT validation) exists. Confusing the two
			// would delete every mail host.
			return OutcomeOK
		default:
			// A timeout is indistinguishable from a filter or a ban.
			return OutcomeError
		}
	}
	return OutcomeError
}

// qualify downgrades a death claimed by a run that could not vouch for itself.
// The observation is still written, but a resolver pool that failed validation
// turns dead hosts into live ones or live ones into timeouts.
func qualify(outcome string, report Report) string {
	if outcome == OutcomeFail && report.RanDegraded() {
		return OutcomeError
	}
	return outcome
}

// writeEvents inserts a whole batch in one statement. Ingestion has a round
// trip budget per observation, enforced by a test; one insert per change would
// blow it on a first run, where everything changes.
func (i *Ingestor) writeEvents(ctx context.Context, q *sqlcgen.Queries, run Run, summary *Summary) error {
	if len(summary.Notifications) == 0 {
		return nil
	}

	batch, err := notify.Batch(run.OrgID, run.ProgramID, i.now(), summary.Notifications)
	if err != nil {
		return err
	}
	for _, event := range summary.Notifications {
		if event.Suppressed {
			summary.Suppressed++
		}
	}

	if _, err := q.WriteEvents(ctx, batch); err != nil {
		return fmt.Errorf("write %d events: %w", len(summary.Notifications), err)
	}
	return nil
}

// accounting carries the scanner's source report into the run's summary.
//
// Every source appears, and a keyed one without its key says so: a run that
// queried two sources of five finds less, and this is the only place it shows.
func accounting(sources []Source) []SourceReport {
	if len(sources) == 0 {
		return nil
	}
	out := make([]SourceReport, 0, len(sources))
	for _, source := range sources {
		out = append(out, SourceReport{
			Name:   source.Name,
			Status: source.Status,
			Found:  source.Found,
			Error:  source.Error,
		})
	}
	return out
}

// Queried summarises the accounting in one line: how much of what could have
// been queried was.
func (s Summary) Queried() string {
	if len(s.Sources) == 0 {
		return ""
	}
	var answered, keyless int
	for _, source := range s.Sources {
		switch source.Status {
		case "ok":
			answered++
		case "skipped_no_key":
			keyless++
		}
	}
	line := fmt.Sprintf("%d of %d sources answered", answered, len(s.Sources))
	if keyless > 0 {
		line += fmt.Sprintf(", %d had no key", keyless)
	}
	return line
}

func discoverySource(run Run, host Host) string {
	if len(host.Sources) > 0 {
		return "fastrecon:" + host.Sources[0]
	}
	if run.Kind == "discovery" {
		return "fastrecon"
	}
	if run.Kind == "candidate" {
		return "certstream"
	}
	return "verification"
}

// lineage records why an asset is in the inventory.
func lineage(run Run, host Host) map[string]any {
	step := map[string]any{"step": "enumerated", "run": run.ID.String()}
	if len(host.Sources) > 0 {
		step["sources"] = anySlice(host.Sources)
	}
	if len(host.Addresses) > 0 {
		step["addresses"] = anySlice(host.Addresses)
	}
	return step
}

// derived records the port that implied a service and the run that found it.
func derived(run Run, key normalize.Key) map[string]any {
	return map[string]any{
		"step": "derived",
		"run":  run.ID.String(),
		"host": key.Host,
		"port": key.Port,
	}
}

func addresses(raw []string) []netip.Addr {
	out := make([]netip.Addr, 0, len(raw))
	for _, item := range raw {
		if addr, err := netip.ParseAddr(item); err == nil {
			out = append(out, addr)
		}
	}
	return out
}

func ports(list []Port) []any {
	out := make([]any, 0, len(list))
	for _, port := range list {
		out = append(out, float64(port.Port))
	}
	return out
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, item := range in {
		out[i] = item
	}
	return out
}

func jsonAny(v any) any {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

func toMap(v any) (map[string]any, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func uuidTo(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func text(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// portPtr narrows an already-bounded port. The check stays: a silently wrapping
// conversion yields a negative port nobody can search for.
func portPtr(n int) *int32 {
	if n < 1 || n > 65535 {
		return nil
	}
	v := int32(n)
	return &v
}

// asnPtr narrows an operator number. It is not portPtr: reusing that dropped
// every four-byte ASN (AS396982, AS132203, most numbers allocated in the last
// decade exceed 65535), leaving the organization set and the number null, so
// operator filters missed assets on newer ranges. Fixtures used AS15169 and
// AS13335 and hid it.
func asnPtr(n int) *int32 {
	if n <= 0 || n > math.MaxInt32 {
		return nil
	}
	v := int32(n)
	return &v
}

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func stampPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return stamp(*t)
}
