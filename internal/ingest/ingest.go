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
	ObsSubmitted     int `json:"observations_submitted"`
	ObsWritten       int `json:"observations_written"`
	// DedupRate is the share of submitted observations that only confirmed the
	// previous one. Expected above 0.9 on a repeated run.
	DedupRate      float64 `json:"dedup_rate"`
	Changes        int     `json:"changes"`
	TooManyPorts   int     `json:"too_many_ports"`
	Degraded       bool    `json:"degraded"`
	SourcesFailing int     `json:"sources_failing"`
}

// Input is a report and the run it answers.
type Input struct {
	Run    sqlcgen.Run
	Apex   string // for a recon: the wildcard's apex, the only perimeter it may report on
	Report *Report
	Now    time.Time
	Log    *slog.Logger
}

type ingester struct {
	in  Input
	w   *inventory.Writer
	sum Summary
	ver string
}

// Ingest writes a recon report into the inventory, inside q's transaction.
func Ingest(ctx context.Context, q *sqlcgen.Queries, in Input) (Summary, error) {
	c, _, err := inventory.Rules(ctx, q, in.Run.ProgramID)
	if err != nil {
		return Summary{}, err
	}
	g := &ingester{
		in:  in,
		w:   &inventory.Writer{Q: q, ProgramID: in.Run.ProgramID, Classifier: c, RunID: &in.Run.ID, Now: in.Now},
		ver: in.Report.Run.Version,
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
	if g.sum.ObsSubmitted > 0 {
		g.sum.DedupRate = float64(g.sum.ObsSubmitted-g.sum.ObsWritten) / float64(g.sum.ObsSubmitted)
	}
	return g.sum, nil
}

func (g *ingester) host(ctx context.Context, h Host) error {
	g.sum.Hosts++
	name, err := canon.FQDN(h.Host)
	if err != nil {
		g.sum.Invalid++
		return nil
	}
	// The scanner is not trusted to stay in its perimeter: a recon reports on
	// its wildcard and nothing else.
	if g.in.Apex != "" && name != g.in.Apex && !strings.HasSuffix(name, "."+g.in.Apex) {
		g.sum.OutsidePerimeter++
		return nil
	}

	outcome, dnsState, observe := qualifyDNS(h, g.sum.Degraded)
	lifecycle := "candidate"
	if h.Status == "live" {
		lifecycle = "active"
	}
	sources := slices.Clone(h.Sources)
	slices.Sort(sources)
	var wildcard *uuid.UUID
	if g.in.Run.WildcardID != nil {
		wildcard = g.in.Run.WildcardID
	}
	host, err := g.w.Upsert(ctx, inventory.Asset{
		ProgramID: g.in.Run.ProgramID, Kind: inventory.KindFQDN, Key: name, Host: name, Wildcard: wildcard,
		Source: "fastrecon", Lifecycle: lifecycle, Sighting: observe,
		Path: []map[string]any{{"step": "enumerated", "run": g.in.Run.ID, "wildcard": g.in.Apex, "sources": sources}},
	})
	if err != nil {
		return err
	}
	if host.New {
		g.sum.AssetsCreated++
	}
	if !observe {
		g.sum.NotReached++
		return nil
	}
	// A recon only promotes: it never concludes a death.
	if !host.New && lifecycle == "active" {
		if err := g.promote(ctx, host.ID); err != nil {
			return err
		}
	}

	dns := normalizeDNS(h)
	if err := g.observe(ctx, host.ID, "dns", outcome, dns); err != nil {
		return err
	}
	var ips []netip.Addr
	for _, a := range dns.Addresses {
		ip, _ := netip.ParseAddr(a)
		ips = append(ips, ip)
		if _, err := g.w.Upsert(ctx, inventory.Asset{
			ProgramID: g.in.Run.ProgramID, Kind: inventory.KindIP, Key: a, Host: a, Source: "fastrecon",
			Lifecycle: "active", Sighting: true,
			Path: []map[string]any{{"step": "resolved", "run": g.in.Run.ID, "from": name}},
		}); err != nil {
			return err
		}
	}

	var cdn *string
	if h.Scan != nil {
		tcp := normalizeTCP(h)
		if len(tcp.CDN) > 0 {
			cdn = &tcp.CDN[0]
		}
		tcpOutcome, tcpState := qualifyTCP(tcp)
		if err := g.observe(ctx, host.ID, "tcp", tcpOutcome, tcp); err != nil {
			return err
		}
		if err := g.w.Q.SetTCPCurrent(ctx, sqlcgen.SetTCPCurrentParams{AssetID: host.ID, TcpState: &tcpState}); err != nil {
			return err
		}
	}
	if err := g.w.Q.SetDNSCurrent(ctx, sqlcgen.SetDNSCurrentParams{
		AssetID: host.ID, DnsState: &dnsState, Ips: ips, Cname: dns.CNAME, Cdn: cdn,
	}); err != nil {
		return err
	}
	if h.Scan == nil {
		return nil
	}

	open := 0
	for _, p := range h.Ports {
		if p.State == "open" && validPort(p.Port) {
			open++
		}
	}
	if open > maxDerivedPorts {
		g.sum.TooManyPorts++
		g.in.Log.WarnContext(ctx, "too many open ports, no service derived", "host", name, "open", open)
		return nil
	}
	for _, p := range h.Ports {
		if p.State != "open" || !validPort(p.Port) {
			continue
		}
		if err := g.service(ctx, host.ID, name, p, ips, cdn); err != nil {
			return err
		}
	}
	return nil
}

// service derives the service behind an open port. Its host is the host of
// the observed asset, never a field of the payload, so a scanner given A cannot
// manufacture services on B.
func (g *ingester) service(ctx context.Context, hostID uuid.UUID, name string, p Port, ips []netip.Addr, cdn *string) error {
	lifecycle := "candidate"
	scheme := ""
	if p.HTTP != nil {
		lifecycle, scheme = "active", strings.ToLower(p.HTTP.Scheme)
	}
	svc, err := g.w.Upsert(ctx, inventory.Asset{
		ProgramID: g.in.Run.ProgramID, Kind: inventory.KindService, Key: inventory.ServiceKey(name, p.Port),
		Host: name, Port: p.Port, Scheme: scheme, Parent: &hostID, Source: "fastrecon",
		Lifecycle: lifecycle, Sighting: true,
		Path: []map[string]any{{"step": "port_open", "run": g.in.Run.ID, "port": p.Port}},
	})
	if err != nil {
		return err
	}
	if svc.New {
		g.sum.ServicesCreated++
	}
	if p.HTTP == nil {
		return nil
	}
	if !svc.New {
		if err := g.promote(ctx, svc.ID); err != nil {
			return err
		}
	}
	d := normalizeHTTP(p.HTTP)
	if err := g.observe(ctx, svc.ID, "http", OK, d); err != nil {
		return err
	}
	state := "responding"
	return g.w.Q.SetHTTPCurrent(ctx, sqlcgen.SetHTTPCurrentParams{
		AssetID: svc.ID, HttpState: &state, StatusCode: optInt32(d.StatusCode), Title: optStr(d.Title),
		Server: optStr(d.Server), FinalUrl: optStr(d.FinalURL), Scheme: optStr(d.Scheme), Ips: ips, Cdn: cdn,
	})
}

// promote moves a candidate to active. Any other state is phase 3's to move.
func (g *ingester) promote(ctx context.Context, id uuid.UUID) error {
	cur, err := g.w.Q.GetAssetCurrent(ctx, id)
	if err != nil {
		return err
	}
	if cur.Lifecycle != "candidate" {
		return nil
	}
	g.sum.Promoted++
	g.sum.Changes++
	return g.w.SetLifecycle(ctx, id, "active")
}

func (g *ingester) observe(ctx context.Context, id uuid.UUID, layer, outcome string, data any) error {
	g.sum.ObsSubmitted++
	res, err := g.w.Observe(ctx, id, layer, outcome, data, g.ver)
	if err != nil {
		return err
	}
	if !res.Inserted {
		return nil
	}
	g.sum.ObsWritten++
	if res.Before == nil {
		return nil
	}
	return g.recordChange(ctx, id, layer, res.Before, data)
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
		cur := after.(TCPData) //nolint:forcetypeassert // the tcp layer always carries TCPData
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
