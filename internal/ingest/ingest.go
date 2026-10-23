package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/canon"
	"github.com/JoshuaMart/recon/internal/inventory"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxDerivedPorts bounds the services derived from one host. Beyond it the
// host is a tarpit or an edge answering for everything: the tcp observation
// keeps the full list, and nothing becomes an asset.
const maxDerivedPorts = 24

// Summary is what ingesting one report did, kept on the run row.
type Summary struct {
	Hosts            int `json:"hosts"`
	OutsidePerimeter int `json:"outside_perimeter"`
	Invalid          int `json:"invalid"`
	NotReached       int `json:"not_reached"`
	AssetsCreated    int `json:"assets_created"`
	ServicesCreated  int `json:"services_created"`
	Promoted         int `json:"promoted"`
	Transitions      int `json:"transitions"`
	ObsSubmitted     int `json:"observations_submitted"`
	ObsWritten       int `json:"observations_written"`
	// DedupRate is the share of submitted observations that only confirmed the
	// previous one. Expected above 0.9 on a repeated run.
	DedupRate    float64 `json:"dedup_rate"`
	Changes      int     `json:"changes"`
	TooManyPorts int     `json:"too_many_ports"`
	// RendersTriggered counts the services and URLs made due for a render.
	RendersTriggered int  `json:"renders_triggered"`
	Degraded         bool `json:"degraded"`
	SourcesFailing   int  `json:"sources_failing"`
}

// Input is a report and the run it answers.
type Input struct {
	Run sqlcgen.Run
	// Apex is a recon's perimeter: its wildcard. Empty for a verification,
	// whose perimeter is its target list.
	Apex       string
	Report     *Report
	Now        time.Time
	Log        *slog.Logger
	Signatures []Signature
	Cadences   *lifecycle.Cadences
	Rand       func() float64
	// Ports is the port list the run swept: a known service on a port outside
	// it was not measured.
	Ports []int
}

func (in Input) verify() bool { return in.Run.Kind == "verification" }

type ingester struct {
	in      Input
	w       *inventory.Writer
	sum     Summary
	ver     string
	targets map[string]bool
	seen    map[string]bool
}

// Ingest writes a report into the inventory, inside q's transaction.
func Ingest(ctx context.Context, q *sqlcgen.Queries, in Input) (Summary, error) {
	c, _, err := inventory.Rules(ctx, q, in.Run.ProgramID)
	if err != nil {
		return Summary{}, err
	}
	g := &ingester{
		in: in,
		w: &inventory.Writer{
			Q: q, ProgramID: in.Run.ProgramID, Classifier: c, RunID: &in.Run.ID, Now: in.Now,
			ProgramActive: true, Cadences: in.Cadences, Rand: in.Rand,
		},
		ver:  in.Report.Run.Version,
		seen: map[string]bool{},
	}
	if in.verify() {
		g.targets = make(map[string]bool, len(in.Run.Targets))
		for _, t := range in.Run.Targets {
			g.targets[t] = true
		}
	}
	g.sum.Degraded = len(in.Report.Run.Degraded) > 0
	for _, s := range in.Report.Sources {
		if s.Status != "ok" && s.Status != "skipped_no_key" {
			g.sum.SourcesFailing++
		}
	}
	for _, h := range in.Report.Hosts {
		if err := g.host(ctx, h); err != nil {
			return Summary{}, fmt.Errorf("host %q: %w", h.Host, err)
		}
	}
	// A target the report does not mention at all (a truncated run) is not
	// reached either: its date moves, nothing is counted.
	for _, t := range in.Run.Targets {
		if !in.verify() || g.seen[t] {
			continue
		}
		g.sum.NotReached++
		a, err := q.GetAssetByKey(ctx, sqlcgen.GetAssetByKeyParams{ProgramID: in.Run.ProgramID, Kind: inventory.KindFQDN, Key: t})
		if err != nil {
			return Summary{}, fmt.Errorf("target %q: %w", t, err)
		}
		if _, _, err := g.settle(ctx, a.ID, inventory.KindFQDN, nil, false); err != nil {
			return Summary{}, err
		}
	}
	if g.sum.ObsSubmitted > 0 {
		g.sum.DedupRate = float64(g.sum.ObsSubmitted-g.sum.ObsWritten) / float64(g.sum.ObsSubmitted)
	}
	return g.sum, nil
}

// inPerimeter is the scanner kept honest: a recon reports on its wildcard, a
// verification on its targets, and nothing else.
func (g *ingester) inPerimeter(name string) bool {
	if g.in.verify() {
		return g.targets[name]
	}
	return name == g.in.Apex || strings.HasSuffix(name, "."+g.in.Apex)
}

func (g *ingester) host(ctx context.Context, h Host) error {
	g.sum.Hosts++
	name, err := canon.FQDN(h.Host)
	if err != nil {
		g.sum.Invalid++
		return nil
	}
	if g.seen[name] {
		return nil // listed twice: the first answer stands
	}
	if !g.inPerimeter(name) {
		g.sum.OutsidePerimeter++
		return nil
	}
	g.seen[name] = true

	outcome, dnsState, observe := qualifyDNS(h, g.sum.Degraded)
	host, err := g.hostAsset(ctx, name, h, observe)
	if err != nil {
		return err
	}
	if !observe {
		g.sum.NotReached++
		if host.New || g.in.verify() {
			_, _, err := g.settle(ctx, host.ID, inventory.KindFQDN, nil, host.New)
			return err
		}
		return nil
	}

	dns := normalizeDNS(h)
	if err := g.observe(ctx, host.ID, "dns", outcome, dns); err != nil {
		return err
	}
	outcomes := map[string]string{"dns": outcome}
	ips, err := g.addresses(ctx, name, dns.Addresses)
	if err != nil {
		return err
	}

	var cdn *string
	var tcp *TCPData
	if h.Scan != nil {
		d := normalizeTCP(h)
		tcp = &d
		if len(d.CDN) > 0 {
			cdn = &d.CDN[0]
		}
		tcpOutcome, tcpState := qualifyTCP(d)
		if err := g.observe(ctx, host.ID, "tcp", tcpOutcome, d); err != nil {
			return err
		}
		outcomes["tcp"] = tcpOutcome
		if err := g.w.Q.SetTCPCurrent(ctx, sqlcgen.SetTCPCurrentParams{AssetID: host.ID, TcpState: &tcpState}); err != nil {
			return err
		}
	}
	if err := g.w.Q.SetDNSCurrent(ctx, sqlcgen.SetDNSCurrentParams{
		AssetID: host.ID, DnsState: &dnsState, Ips: ips, Cname: dns.CNAME, Cdn: cdn,
	}); err != nil {
		return err
	}
	_, after, err := g.settle(ctx, host.ID, inventory.KindFQDN, outcomes, host.New)
	if err != nil {
		return err
	}
	if after == lifecycle.Inactive {
		// A service has no schedule of its own: it dies with its host, or it
		// would stay active on a name that is gone.
		if err := g.followHost(ctx, host.ID); err != nil {
			return err
		}
	}
	if tcp == nil {
		return nil
	}
	return g.services(ctx, host.ID, name, h, *tcp, ips, cdn)
}

func (g *ingester) hostAsset(ctx context.Context, name string, h Host, observe bool) (inventory.Written, error) {
	if g.in.verify() {
		// A target comes from the inventory, so the asset exists.
		a, err := g.w.Q.GetAssetByKey(ctx, sqlcgen.GetAssetByKeyParams{ProgramID: g.in.Run.ProgramID, Kind: inventory.KindFQDN, Key: name})
		if err != nil {
			return inventory.Written{}, err
		}
		return inventory.Written{ID: a.ID, Scope: a.ScopeStatus}, nil
	}
	lifecycleState := lifecycle.Candidate
	if h.Status == "live" {
		lifecycleState = lifecycle.Active
	}
	sources := slices.Clone(h.Sources)
	slices.Sort(sources)
	w, err := g.w.Upsert(ctx, inventory.Asset{
		ProgramID: g.in.Run.ProgramID, Kind: inventory.KindFQDN, Key: name, Host: name, Wildcard: g.in.Run.WildcardID,
		Source: "fastrecon", Lifecycle: lifecycleState, Sighting: observe,
		Path: []map[string]any{{"step": "enumerated", "run": g.in.Run.ID, "wildcard": g.in.Apex, "sources": sources}},
	})
	if err == nil && w.New {
		g.sum.AssetsCreated++
	}
	return w, err
}

func (g *ingester) addresses(ctx context.Context, from string, addrs []string) ([]netip.Addr, error) {
	ips := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		ip, _ := netip.ParseAddr(a)
		ips = append(ips, ip)
		if _, err := g.w.Upsert(ctx, inventory.Asset{
			ProgramID: g.in.Run.ProgramID, Kind: inventory.KindIP, Key: a, Host: a, Source: "fastrecon",
			Lifecycle: lifecycle.Active, Sighting: true,
			Path: []map[string]any{{"step": "resolved", "run": g.in.Run.ID, "from": from}},
		}); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// services handles the ports of a host whose sweep ran: open ports become or
// confirm services, and in a verification a known service whose port is gone
// is measured as such.
func (g *ingester) services(ctx context.Context, hostID uuid.UUID, name string, h Host, tcp TCPData, ips []netip.Addr, cdn *string) error {
	if len(tcp.OpenPorts) > maxDerivedPorts {
		g.sum.TooManyPorts++
		g.in.Log.WarnContext(ctx, "too many open ports, no service derived", "host", name, "open", len(tcp.OpenPorts))
		return nil
	}
	byPort := map[int]Port{}
	for _, p := range h.Ports {
		if p.State == "open" && validPort(p.Port) {
			byPort[p.Port] = p
		}
	}
	for _, port := range tcp.OpenPorts {
		if err := g.service(ctx, hostID, name, byPort[port], ips, cdn); err != nil {
			return err
		}
	}
	if !g.in.verify() {
		return nil
	}
	known, err := g.w.Q.ListChildServices(ctx, &hostID)
	if err != nil {
		return err
	}
	// Refusals are counted per host: only a sweep where every port that is not
	// open was refused says that a given port was, and only for a port it
	// tried. A CDN address is swept on 80 and 443 alone.
	clean := tcp.Filtered == 0 && tcp.Unknown == 0 && tcp.Refused == tcp.Scanned-tcp.Open
	swept := g.in.Ports
	if tcp.ScanLimited {
		swept = []int{80, 443}
	}
	for _, s := range known {
		if s.Port == nil || slices.Contains(tcp.OpenPorts, int(*s.Port)) || !slices.Contains(swept, int(*s.Port)) {
			continue
		}
		outcome, data := Error, map[string]any{"port": "not_seen"}
		if clean {
			outcome, data = Fail, map[string]any{"port": "refused"}
		}
		if err := g.observe(ctx, s.ID, "http", outcome, data); err != nil {
			return err
		}
		if _, _, err := g.settle(ctx, s.ID, inventory.KindService, map[string]string{"http": outcome}, false); err != nil {
			return err
		}
	}
	return nil
}

// service derives or confirms the service behind an open port. Its host is the
// host of the observed asset, never a field of the payload, so a scanner given
// A cannot manufacture services on B.
func (g *ingester) service(ctx context.Context, hostID uuid.UUID, name string, p Port, ips []netip.Addr, cdn *string) error {
	state, scheme := lifecycle.Candidate, ""
	if p.HTTP != nil {
		state, scheme = lifecycle.Active, strings.ToLower(p.HTTP.Scheme)
	}
	svc, err := g.w.Upsert(ctx, inventory.Asset{
		ProgramID: g.in.Run.ProgramID, Kind: inventory.KindService, Key: inventory.ServiceKey(name, p.Port),
		Host: name, Port: p.Port, Scheme: scheme, Parent: &hostID, Source: "fastrecon",
		Lifecycle: state, Sighting: true,
		Path: []map[string]any{{"step": "port_open", "run": g.in.Run.ID, "port": p.Port}},
	})
	if err != nil {
		return err
	}
	if svc.New {
		g.sum.ServicesCreated++
	}
	if p.HTTP == nil {
		// The port is open and nothing speaks HTTP on it, a database or a
		// broker: nothing conclusive about a web service, and the same answer
		// every time so it deduplicates.
		if err := g.observe(ctx, svc.ID, "http", Error, map[string]any{"http": "no_answer"}); err != nil {
			return err
		}
		_, _, err := g.settle(ctx, svc.ID, inventory.KindService, map[string]string{"http": Error}, svc.New)
		return err
	}
	d := normalizeHTTP(p.HTTP)
	outcome := OK
	if DeadOrigin(g.in.Signatures, d) {
		outcome = Fail
	}
	prev, err := g.w.Q.GetAssetCurrent(ctx, svc.ID)
	if err != nil {
		return err
	}
	changed, err := g.observeChanged(ctx, svc.ID, "http", outcome, d)
	if err != nil {
		return err
	}
	// First contact, or the HTTP layer moved: the browser looks again, once
	// the HTTP state is written below.
	render := prev.HttpState == nil || changed
	if _, _, err := g.settle(ctx, svc.ID, inventory.KindService, map[string]string{"http": outcome}, svc.New); err != nil {
		return err
	}
	httpState := "responding"
	if outcome == Fail {
		httpState = "dead_origin"
	}
	if err := g.w.Q.SetHTTPCurrent(ctx, sqlcgen.SetHTTPCurrentParams{
		AssetID: svc.ID, HttpState: &httpState, StatusCode: optInt32(d.StatusCode), Title: optStr(d.Title),
		Server: optStr(d.Server), FinalUrl: optStr(d.FinalURL), Scheme: optStr(d.Scheme), Ips: ips, Cdn: cdn,
	}); err != nil {
		return err
	}
	if !render {
		return nil
	}
	// The trigger checks scope and program state itself.
	return g.triggerRender(ctx, svc.ID)
}

func (g *ingester) settle(ctx context.Context, id uuid.UUID, kind string, outcomes map[string]string, isNew bool) (before, after string, err error) {
	before, after, err = g.w.Settle(ctx, inventory.Settle{
		ID: id, Kind: kind, Outcomes: outcomes, Verify: g.in.verify(), Stage: g.in.Run.Stage, New: isNew,
	})
	if err != nil {
		return "", "", err
	}
	g.counted(before, after)
	return before, after, nil
}

func (g *ingester) counted(before, after string) {
	if before == after {
		return
	}
	g.sum.Transitions++
	g.sum.Changes++
	if after == lifecycle.Active {
		g.sum.Promoted++
	}
}

// followHost makes a dead host's services inactive with it.
func (g *ingester) followHost(ctx context.Context, hostID uuid.UUID) error {
	children, err := g.w.Q.ListChildServices(ctx, &hostID)
	if err != nil {
		return err
	}
	for _, c := range children {
		cur, err := g.w.Q.GetAssetCurrent(ctx, c.ID)
		if err != nil {
			return err
		}
		if cur.Lifecycle == lifecycle.Inactive || cur.Lifecycle == lifecycle.Archived {
			continue
		}
		if err := g.w.SetLifecycle(ctx, c.ID, lifecycle.Inactive); err != nil {
			return err
		}
		g.counted(cur.Lifecycle, lifecycle.Inactive)
	}
	return nil
}

func (g *ingester) observe(ctx context.Context, id uuid.UUID, layer, outcome string, data any) error {
	_, err := g.observeChanged(ctx, id, layer, outcome, data)
	return err
}

// observeChanged writes an observation and reports whether it differs from a
// previous one.
func (g *ingester) observeChanged(ctx context.Context, id uuid.UUID, layer, outcome string, data any) (bool, error) {
	g.sum.ObsSubmitted++
	res, err := g.w.Observe(ctx, id, layer, outcome, data, g.ver)
	if err != nil {
		return false, err
	}
	if !res.Inserted {
		return false, nil
	}
	g.sum.ObsWritten++
	if res.Before == nil {
		return false, nil
	}
	return true, g.recordChange(ctx, id, layer, res.Before, data)
}

// triggerRender makes a web service and the URLs declared on it due for a
// render, at normal priority.
func (g *ingester) triggerRender(ctx context.Context, id uuid.UUID) error {
	n, err := g.w.Q.TriggerRender(ctx, sqlcgen.TriggerRenderParams{AssetID: id, Priority: 1, Now: g.in.Now})
	if err != nil {
		return err
	}
	g.sum.RendersTriggered += int(n)
	m, err := g.w.Q.TriggerChildURLs(ctx, sqlcgen.TriggerChildURLsParams{ServiceID: &id, Priority: 1, Now: g.in.Now})
	g.sum.RendersTriggered += int(m)
	return err
}

// recordChange writes the change between two successive observations of a
// layer. A new port is its own change, so it can be filtered on.
func (g *ingester) recordChange(ctx context.Context, id uuid.UUID, layer string, before json.RawMessage, after any) error {
	switch layer {
	case "tcp":
		var old TCPData
		if err := json.Unmarshal(before, &old); err != nil {
			return err
		}
		cur, ok := after.(TCPData)
		if !ok {
			return nil
		}
		for _, p := range cur.OpenPorts {
			if !slices.Contains(old.OpenPorts, p) {
				g.sum.Changes++
				if err := g.w.Change(ctx, id, "port_opened", layer, nil, map[string]any{"port": p}); err != nil {
					return err
				}
			}
		}
		for _, p := range old.OpenPorts {
			if !slices.Contains(cur.OpenPorts, p) {
				g.sum.Changes++
				if err := g.w.Change(ctx, id, "port_closed", layer, map[string]any{"port": p}, nil); err != nil {
					return err
				}
			}
		}
		return nil
	case "dns":
		g.sum.Changes++
		return g.w.Change(ctx, id, "dns_changed", layer, before, after)
	case "http":
		g.sum.Changes++
		return g.w.Change(ctx, id, "http_changed", layer, before, after)
	}
	return nil
}

func optInt32(v int) *int32 {
	if v == 0 {
		return nil
	}
	n := int32(v) //nolint:gosec // an HTTP status code
	return &n
}

func optStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
