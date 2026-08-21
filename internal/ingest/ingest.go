package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JoshuaMart/recon/internal/enrich"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/scope"
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
	Derived      int
	Unknown      map[string]int
}

// Ingestor writes reports into the inventory.
type Ingestor struct {
	enricher enrich.Enricher
	now      func() time.Time
	log      *slog.Logger
}

// New builds an ingestor.
func New(enricher enrich.Enricher, log *slog.Logger) *Ingestor {
	if enricher == nil {
		enricher = enrich.Nothing()
	}
	return &Ingestor{enricher: enricher, now: time.Now, log: log}
}

// Report writes a whole report inside one transaction.
//
// Everything that concludes something about a target is decided here rather
// than believed: the scope, the outcome, and which assets a finding implies. A
// scanner that lied about any of the three would be reclassified, requalified
// and bounded on arrival.
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

		assetID, err := i.writeAsset(ctx, q, run, set, key, host, nil)
		if err != nil {
			return summary, err
		}
		summary.Assets++

		if err := i.writeHostObservations(ctx, q, run, report, assetID, host, &summary); err != nil {
			return summary, err
		}
		if err := i.writeServices(ctx, q, run, set, report, key, assetID, host, &summary); err != nil {
			return summary, err
		}
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

func (i *Ingestor) writeAsset(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set,
	key normalize.Key, host Host, parent *uuid.UUID,
) (uuid.UUID, error) {
	target := scope.Target{Key: key, Addresses: addresses(host.Addresses)}
	status := set.Classify(target)

	path, err := lineage(run, host)
	if err != nil {
		return uuid.Nil, err
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
		params.Port = int32Ptr(key.Port)
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
	// Only in-scope assets are scheduled, which the statement enforces too.
	if status == scope.InScope {
		params.NextResolveAt = stampPtr(run.Due.Resolve)
		params.NextFullAt = stampPtr(run.Due.Full)
	}

	row, err := q.UpsertAssetAndProjection(ctx, params)
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert asset %s: %w", key.Value, err)
	}
	return uuid.UUID(row.AssetID.Bytes), nil
}

func (i *Ingestor) writeHostObservations(
	ctx context.Context, q *sqlcgen.Queries, run Run, report Report,
	assetID uuid.UUID, host Host, summary *Summary,
) error {
	// A host the run never reached produces nothing. Inventing a verdict for
	// one that was never queried would be worse than admitting the gap, and it
	// is how a truncated run archives live assets.
	if host.Status == StatusDiscovered {
		return nil
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
	if err := i.write(ctx, q, run, report, assetID, normalize.LayerDNS, dnsOutcome(host), dns, summary); err != nil {
		return err
	}

	if len(host.Ports) > 0 {
		tcp := map[string]any{"open_ports": ports(host.Ports)}
		if len(host.Addresses) > 0 {
			tcp["addresses"] = anySlice(host.Addresses)
		}
		if len(host.CDN) > 0 {
			tcp["cdn"] = jsonAny(host.CDN)
		}
		if err := i.write(ctx, q, run, report, assetID, normalize.LayerTCP, OutcomeOK, tcp, summary); err != nil {
			return err
		}
	}

	return nil
}

// writeServices turns every open port into an asset.
//
// Without it the port scan buys nothing: no asset means no due date, no HTTP
// probe on that port, no service detected, and nothing reaching the renderer.
// A forgotten application on an unusual port is the reason to scan, and the
// probe that finds it would have no way to put it in the inventory.
func (i *Ingestor) writeServices(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, report Report,
	hostKey normalize.Key, hostID uuid.UUID, host Host, summary *Summary,
) error {
	// Only a host derives services. A service makes its own port scanned and
	// nothing else, so deriving from one would recreate itself.
	if hostKey.Kind != normalize.KindFQDN && hostKey.Kind != normalize.KindIP {
		return nil
	}
	if len(host.Ports) > maxDerivedPorts {
		i.log.WarnContext(ctx, "too many open ports to derive services",
			"host", hostKey.Value, "open", len(host.Ports), "bound", maxDerivedPorts)
		return nil
	}

	for _, port := range host.Ports {
		// The host of the derived key is the host of the observed asset, never
		// a field of the payload. A scanner given one target cannot manufacture
		// services on another.
		key, err := normalize.Service(hostKey.Host, port.Port, port.Protocol)
		if err != nil {
			summary.Skipped++
			continue
		}

		serviceID, err := i.writeAsset(ctx, q, run, set, key, host, &hostID)
		if err != nil {
			return err
		}
		summary.Assets++
		summary.Derived++

		tcp := map[string]any{"open_ports": []any{float64(port.Port)}}
		if len(port.Addresses) > 0 {
			tcp["addresses"] = anySlice(port.Addresses)
		}
		if err := i.write(ctx, q, run, report, serviceID, normalize.LayerTCP, OutcomeOK, tcp, summary); err != nil {
			return err
		}

		if port.HTTP != nil {
			payload, err := toMap(port.HTTP)
			if err != nil {
				return err
			}
			if err := i.write(ctx, q, run, report, serviceID, normalize.LayerHTTP, OutcomeOK, payload, summary); err != nil {
				return err
			}
		}
	}

	return nil
}

// write normalizes and stores one observation.
func (i *Ingestor) write(
	ctx context.Context, q *sqlcgen.Queries, run Run, report Report,
	assetID uuid.UUID, layer normalize.Layer, outcome string,
	data map[string]any, summary *Summary,
) error {
	result, err := normalize.Payload(layer, data)
	if err != nil {
		return fmt.Errorf("normalize %s: %w", layer, err)
	}
	for _, name := range result.Unknown {
		summary.Unknown[string(layer)+"."+name]++
	}

	encoded, err := json.Marshal(result.Data)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", layer, err)
	}

	params := sqlcgen.WriteObservationParams{
		OrgID:      uuidTo(run.OrgID),
		AssetID:    uuidTo(assetID),
		ObservedAt: stamp(i.now()),
		RunID:      uuidTo(run.ID),
		Source:     "fastrecon",
		Layer:      string(layer),
		Outcome:    qualify(outcome, report),
		Data:       encoded,
	}
	if report.Run.Version != "" {
		params.ProducerVersion = text(report.Run.Version)
	}

	row, err := q.WriteObservation(ctx, params)
	if err != nil {
		return fmt.Errorf("write %s observation: %w", layer, err)
	}

	summary.Observations++
	if row.Deduplicated {
		summary.Deduplicated++
	}
	return nil
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
func lineage(run Run, host Host) ([]byte, error) {
	step := map[string]any{"step": "enumerated", "run": run.ID.String()}
	if len(host.Sources) > 0 {
		step["sources"] = anySlice(host.Sources)
	}
	if len(host.Addresses) > 0 {
		step["addresses"] = anySlice(host.Addresses)
	}
	return json.Marshal([]any{step})
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

// int32Ptr narrows a port, which the caller has already bounded to 1..65535.
// The check is here anyway: a conversion that silently wraps is the kind of
// thing that turns a port into a negative number nobody can search for.
func int32Ptr(n int) *int32 {
	if n < 0 || n > 65535 {
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
