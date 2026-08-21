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
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/signals"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxDerivedPorts bounds what one host may imply.
//
// A host answering on a quarter of the curated list does not have twenty-five
// services: it is a tarpit, a device accepting every connection, or an edge
// answering for everything behind it. Creating an asset each would turn an
// inventory into noise, and a first scan of a perimeter holding ten such hosts
// would produce a thousand. The observation keeps its full port list either
// way, so the finding stays readable even when it does not become an asset.
const maxDerivedPorts = 24

// ErrOutOfScope is returned when a report names a host the run was not given.
// Rejected rather than ignored: it is what stops a scanner choosing its own
// perimeter.
var ErrOutOfScope = errors.New("host is outside the run's frozen target list")

// Run is what the caller knows about the execution that produced a report.
type Run struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	ProgramID uuid.UUID
	Kind      string
	// Scope is the rung the run reached, and it decides which due dates its
	// report moves.
	Scope string
	// Targets is the frozen list, keyed by canonical host. Nil on a discovery
	// run, which has no list because it is the one allowed to find things.
	Targets map[string]struct{}
	// Due is when a freshly created asset becomes due. The arithmetic lives in
	// Go, where it is testable, rather than in the statement that stores it.
	Due Schedule
}

// Schedule is the first due date of a new asset.
type Schedule struct {
	Resolve *time.Time
	Full    *time.Time
}

// Summary is what one report changed.
type Summary struct {
	Hosts        int
	Assets       int
	Created      int
	Observations int
	Deduplicated int
	Rejected     int
	Skipped      int
	// Late marks a report that arrived after its run's deadline. The data is
	// still valid, so it is recorded rather than refused: the run may simply
	// have been re-dispatched, and deduplication merges the two.
	Late    bool
	Derived int
	// Takeovers counts the findings this report carried, so a run that found
	// one says so in its own summary rather than only in a table nobody reads
	// until an alert fires.
	Takeovers int
	// Sources is the per source accounting the scanner reported, kept because
	// the failure it describes does not look like one. Without a credential a
	// keyed source disables itself, the run starts, finishes correctly, and
	// simply finds less, and nobody notices while looking at an inventory they
	// have never seen otherwise.
	Sources []SourceReport `json:"sources,omitempty"`
	Unknown map[string]int
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
	// random draws the jitter every delay is widened by. Without it the
	// thousands of assets one run wrote share a due date and come back
	// together forever. It is a field so that a test asserts a date rather
	// than a range.
	random func() float64
	now    func() time.Time
	log    *slog.Logger
}

// Option adjusts an ingestor.
//
// Two of the three inputs of a due date are a clock and a random draw, which
// makes the arithmetic of scheduling untestable unless both can be handed in.
// Asserting a range instead would pass on a function that returns a constant.
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
// Everything that concludes something about a target is decided here rather
// than believed: the scope, the outcome, which assets a finding implies, and
// what state the asset is now in. A scanner that lied about any of them would
// be reclassified, requalified and bounded on arrival.
func (i *Ingestor) Report(ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, report Report) (Summary, error) {
	summary := Summary{Unknown: map[string]int{}}

	for _, host := range report.Hosts {
		summary.Hosts++

		key, err := hostKey(host.Host)
		if err != nil {
			// A source can return junk. One unusable name is not a reason to
			// refuse a report, but it is counted rather than swallowed.
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

		// Whether an asset sits behind an edge is structural, seen identically
		// by every observer, so it is decided once for the host and carried to
		// the services it derives.
		edge := i.edge(host)

		answered, err := i.writeHostObservations(ctx, q, run, report, st, host, edge, &summary)
		if err != nil {
			return summary, err
		}
		if err := i.writeServices(ctx, q, run, set, report, key, st, host, edge, &summary); err != nil {
			return summary, err
		}

		// A host the report did not answer for keeps its due date, so the next
		// tick selects it again. Silence is not a measurement, and turning it
		// into one is how a truncated run archives live assets.
		if !answered {
			continue
		}
		if err := i.reschedule(ctx, q, run, st); err != nil {
			return summary, err
		}
	}

	summary.Sources = accounting(report.Sources)

	// A declared path earns its render once the service it belongs to has
	// answered. One statement for the whole report rather than one per
	// service: it reads a state the observations above have already written.
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
// address asset, not a name: the report does not say which it sent.
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

	// A pass that had nothing to look at has no opinion, which is not the same
	// as an opinion that the asset is not fronted. A resolution that timed out
	// carries no address, no CNAME and no provider, and writing false from it
	// would clear the flag and the provider on a fronted asset every time the
	// name failed to answer.
	//
	// A name that resolved is a name this looked at, whatever the enrichment
	// could add: the scanner's own determination and the terminal CNAME are
	// both real evidence, and the operator is the third source rather than the
	// first.
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

	// A derived service was not returned by an enumeration source: it was
	// implied by a port its host was found to have open. Copying the host's
	// step would attribute it to whichever source returned the name, and a
	// question like "what did this source actually find" would answer with
	// every service in the inventory.
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
	// Only in-scope assets are scheduled, and only hosts carry these two. A
	// service is observed through its host's run, so a resolve date on one
	// would put it in a queue nothing dispatches from.
	if status == scope.InScope && (key.Kind == normalize.KindFQDN || key.Kind == normalize.KindIP) {
		params.NextResolveAt = stampPtr(run.Due.Resolve)
		params.NextFullAt = stampPtr(run.Due.Full)
	}

	row, err := q.UpsertAssetAndProjection(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("upsert asset %s: %w", key.Value, err)
	}
	return newState(row, key.Kind, key.Port)
}

// enrichInto turns the address a run connected to into an operator and a place.
//
// It reads the submitted payload rather than the normalized one, which is the
// only exception of its kind in the write path and is deliberate: a name in
// round robin answers a different address on every pass, so the field is
// dropped at normalization and exists only to fill these columns. A payload
// carrying it would differ on every run for a target nobody touched.
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

// writeHostObservations writes what a run learned about a name. It reports
// whether the run answered for the host at all.
func (i *Ingestor) writeHostObservations(
	ctx context.Context, q *sqlcgen.Queries, run Run, report Report,
	st *state, host Host, edge promoted, summary *Summary,
) (bool, error) {
	// A host the run never reached produces nothing. Inventing a verdict for
	// one that was never queried would be worse than admitting the gap, and it
	// is how a truncated run archives live assets.
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

	// A name pointing at a name that no longer exists. The dangling pointer
	// and its proof arrive in one query, because a recursive resolver follows
	// the chain itself and returns nxdomain with the CNAME still in the answer
	// section.
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
		// No takeover kind. A finding is cleared only by the layer that could
		// have produced it, and this one produces none: claiming the orphan
		// CNAME kind here let a tcp observation delete, in the same report,
		// the finding the dns observation had just written.
	}, summary); err != nil {
		return true, err
	}

	return true, nil
}

// writeServices turns every open port into an asset.
//
// Without it the port scan buys nothing: no asset means no due date, no HTTP
// probe on that port, no service detected, and nothing reaching the renderer.
// A forgotten application on an unusual port is the reason to scan, and the
// probe that finds it would have no way to put it in the inventory.
func (i *Ingestor) writeServices(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, report Report,
	hostKey normalize.Key, host *state, reported Host, edge promoted, summary *Summary,
) error {
	// Only a host derives services. A service makes its own port scanned and
	// nothing else, so deriving from one would recreate itself.
	if hostKey.Kind != normalize.KindFQDN && hostKey.Kind != normalize.KindIP {
		return nil
	}
	if len(reported.Ports) > maxDerivedPorts {
		i.log.WarnContext(ctx, "too many open ports to derive services",
			"host", hostKey.Value, "open", len(reported.Ports), "bound", maxDerivedPorts)
		return nil
	}

	for _, port := range reported.Ports {
		// The host of the derived key is the host of the observed asset, never
		// a field of the payload. A scanner given one target cannot manufacture
		// services on another.
		key, err := normalize.Service(hostKey.Host, port.Port, port.Protocol)
		if err != nil {
			summary.Skipped++
			continue
		}

		// The scheme is what answered on the port, and it is what a render is
		// pointed at later. Deriving it from the port number would be a guess
		// about every application that speaks TLS somewhere unusual, and taking
		// it from the report as written would let "HTTPS" through: the scheme
		// decides whether a port belongs in an authority, so a case nobody
		// lowered points a browser at a different service on the same host.
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

		// A service earns its first render on transport reachability and
		// nothing else. It is deliberately not a filter on the outcome: an
		// origin error behind a CDN is an informative failure counted as proof
		// of death, and it still deserves a baseline, because an edge
		// answering for a dead origin is a page with something to read.
		//
		// What it does read is the instrument. Chrome answers ERR_UNSAFE_PORT
		// on its own restricted list, so the failure is certain before the
		// call, and a certain failure is not a measurement. The protocol is
		// part of that: a browser speaks no UDP, and the render queue carries
		// no protocol to notice with later.
		if key.Kind == normalize.KindService && port.Protocol != "udp" && fingerprint.Renderable(port.Port) {
			if err := i.earnBaseline(ctx, q, service); err != nil {
				return err
			}
		}

		// A derived service stays a candidate until something addresses the
		// service itself. The port scan is an observation about the host, and
		// reading it as one about the service would report every open port as
		// a verified application.
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

	// The presence of a response is not liveness. On a fronted target the edge
	// always answers, with no refusal and no nxdomain, so an asset whose origin
	// is dead would stay active forever. Death there is readable only in the
	// semantics of the response.
	outcome := OutcomeOK
	if verdict.Dead != "" {
		outcome = OutcomeFail
		payload["origin_dead"] = verdict.Dead
	}

	// Orthogonal to the outcome. A 403 carrying a mitigation signature is a
	// target that answered and is there, and a probe that learned nothing.
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

// earnBaseline gives a service its first render date.
//
// It applies only where there is none, so a service already queued keeps its
// place and a baseline is not re-armed on every pass over the same port.
//
// A baseline is due when it is earned. It does not inherit the discovery
// jitter: that spread exists for the first probe of a freshly discovered asset,
// and this line is created later, once an observation has proved the target
// worth rendering. The herd has already been spread once.
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

// tcpOutcome reads what the port sweep proved, and it is empty when the sweep
// proved nothing.
//
// A report that lists only open ports cannot conclude a death: an empty list is
// "nothing was open" and "nothing was tried" at once, and those are opposite
// findings. The counts are what separate them, and until a report carries them
// this layer writes nothing rather than a verdict it cannot support.
func tcpOutcome(host Host) string {
	if len(host.Ports) > 0 {
		return OutcomeOK
	}
	if host.Scan == nil || !host.Scan.Accounted() {
		return ""
	}
	// The host answers and nothing listens. Four things break it. Three are a
	// different way of not having tried: a filtered port is indistinguishable
	// from a banned one and could be hiding a service, and an unknown one was
	// never measured at all. The fourth is the counts contradicting the port
	// list, which the walk above already returned on, and which a report this
	// does not trust must not be able to turn into a death anyway.
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

// Outcomes, which are not "succeeded, failed, crashed" but what was learned
// about the target: it answered and it is there, it answered and it is not, or
// nothing conclusive was obtained.
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
			// The only clean death signal there is.
			return OutcomeFail
		case ReasonNoAnswer:
			// A name that exists without an address, an MX-only host or a TXT
			// validation record, is not a name that does not exist. Confusing
			// the two would delete every mail host from an inventory.
			return OutcomeOK
		default:
			// A timeout is indistinguishable from a filter or a ban.
			return OutcomeError
		}
	}
	return OutcomeError
}

// qualify downgrades a death claimed by a run that could not vouch for itself.
//
// The observation is still written, because it happened, but a resolver pool
// that failed validation turns every dead host into a live one or every live
// host into a timeout. A degraded observer is an observer, not a verdict.
func qualify(outcome string, report Report) string {
	if outcome == OutcomeFail && report.RanDegraded() {
		return OutcomeError
	}
	return outcome
}

// accounting carries the scanner's source report into the run's summary.
//
// Every source appears whether it succeeded or not, and a keyed one with no key
// says so rather than being silently dropped. That distinction is the whole
// point: a run that queried two sources out of five is a run that found less,
// and the only place that is visible is here.
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

// Queried summarises the accounting in one line.
//
// It is a sentence rather than a table because the question it answers is asked
// while looking at a disappointing inventory: how much of what could have been
// queried actually was.
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
	return "verification"
}

// lineage records why an asset is in the inventory, which matters for
// debugging, for trust, and for justifying a scan to whoever owns the target.
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

// derived records the port that implied a service, and the run that found it.
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

// portPtr narrows a port, which the caller has already bounded. The check is
// here anyway: a conversion that silently wraps turns a port into a negative
// number nobody can search for.
func portPtr(n int) *int32 {
	if n < 1 || n > 65535 {
		return nil
	}
	v := int32(n)
	return &v
}

// asnPtr narrows an operator number, and it is deliberately not the function
// above.
//
// Reusing the port helper here dropped every four-byte ASN in silence: AS396982
// is Google Cloud, AS132203 is Tencent, and most numbers allocated in the last
// decade are above 65535. The organization and the country were written while
// the number was left null, so every filter on an operator missed exactly the
// assets hosted on the newer ranges. Nothing caught it because the fixtures
// used AS15169 and AS13335.
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
