package ingest

import (
	"net/netip"
	"slices"
	"strings"

	"github.com/JoshuaMart/recon/internal/canon"
)

// The observation payloads, one per layer. They are what is compared for
// deduplication, so they hold only what describes the target: arrays whose
// order means nothing are sorted, and fields that change on every request are
// left out.

// DNSData is the dns layer of a host.
type DNSData struct {
	Status    string   `json:"status"`
	Reason    string   `json:"reason,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	// CNAME keeps its order: it is a chain, read from the name to the target.
	CNAME []string `json:"cname,omitempty"`
}

// TCPData is the tcp layer of a host. The scanned port list is the probe's
// settings, so only counts are kept, which still tell "nothing else is open"
// from "nothing else was tried".
type TCPData struct {
	Scanned     int      `json:"scanned"`
	Open        int      `json:"open"`
	Refused     int      `json:"refused"`
	Filtered    int      `json:"filtered"`
	Unknown     int      `json:"unknown"`
	OpenPorts   []int    `json:"open_ports,omitempty"`
	ScanLimited bool     `json:"scan_limited,omitempty"`
	CDN         []string `json:"cdn,omitempty"`
}

// HTTPData is the http layer of a service. Response time and content length
// measure the request, not the service, and are dropped.
type HTTPData struct {
	Scheme             string   `json:"scheme"`
	StatusCode         int      `json:"status_code"`
	Title              string   `json:"title,omitempty"`
	Server             string   `json:"server,omitempty"`
	FinalURL           string   `json:"final_url,omitempty"`
	Redirects          []string `json:"redirects,omitempty"` // a chain: order kept
	RedirectUnfollowed bool     `json:"redirect_unfollowed,omitempty"`
	TLS                *TLSData `json:"tls,omitempty"`
}

// TLSData is the certificate an endpoint presented.
type TLSData struct {
	SubjectCN    string   `json:"subject_cn,omitempty"`
	Issuer       string   `json:"issuer,omitempty"`
	NotAfter     string   `json:"not_after,omitempty"`
	SANs         []string `json:"sans,omitempty"`
	CertSPKIHash string   `json:"cert_spki_hash,omitempty"`
}

func normalizeDNS(h Host) DNSData {
	d := DNSData{Status: h.Status, Reason: h.Reason, Addresses: addresses(h.Addresses)}
	for _, c := range h.CNAME {
		if n, err := canon.FQDN(c); err == nil {
			d.CNAME = append(d.CNAME, n)
		}
	}
	return d
}

func normalizeTCP(h Host) TCPData {
	d := TCPData{
		Scanned: h.Scan.Scanned, Open: h.Scan.Open, Refused: h.Scan.Refused,
		Filtered: h.Scan.Filtered, Unknown: h.Scan.Unknown,
	}
	for _, p := range h.Ports {
		if p.State == "open" && validPort(p.Port) {
			d.OpenPorts = append(d.OpenPorts, p.Port)
		}
	}
	slices.Sort(d.OpenPorts)
	d.OpenPorts = slices.Compact(d.OpenPorts)
	for _, c := range h.CDN {
		d.CDN = append(d.CDN, strings.ToLower(c.Name))
		d.ScanLimited = d.ScanLimited || c.ScanLimited
	}
	slices.Sort(d.CDN)
	d.CDN = slices.Compact(d.CDN)
	return d
}

func normalizeHTTP(h *HTTP) HTTPData {
	d := HTTPData{
		Scheme: strings.ToLower(h.Scheme), StatusCode: h.StatusCode, Title: strings.TrimSpace(h.Title),
		Server: strings.TrimSpace(h.Server), RedirectUnfollowed: h.RedirectUnfollowed,
	}
	if u, err := canon.URL(h.FinalURL); err == nil {
		d.FinalURL = u
	}
	for _, r := range h.Redirects {
		if u, err := canon.URL(r); err == nil {
			d.Redirects = append(d.Redirects, u)
		}
	}
	if t := h.TLS; t != nil {
		td := &TLSData{
			SubjectCN: strings.ToLower(t.SubjectCN), Issuer: t.Issuer, NotAfter: t.NotAfter,
			CertSPKIHash: strings.ToLower(t.CertSPKIHash),
		}
		for _, s := range t.SANs {
			td.SANs = append(td.SANs, strings.ToLower(strings.TrimSuffix(s, ".")))
		}
		slices.Sort(td.SANs)
		td.SANs = slices.Compact(td.SANs)
		d.TLS = td
	}
	return d
}

// addresses canonicalizes, sorts and deduplicates, dropping what does not parse.
func addresses(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if ip, err := netip.ParseAddr(a); err == nil {
			out = append(out, ip.Unmap().String())
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func validPort(p int) bool { return p > 0 && p <= 65535 }
