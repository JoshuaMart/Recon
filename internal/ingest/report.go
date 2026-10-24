// Package ingest turns a FastRecon report into inventory: assets, normalized
// and deduplicated observations, and changes. The scanner is not trusted: the
// hosts are checked against the run's perimeter and every outcome is derived
// here from the data, never taken from a label.
package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Report is FastRecon's report, schema 1.x. Fields Recon does not use are left
// out; unknown fields are ignored so a minor schema bump is not an outage.
type Report struct {
	SchemaVersion string   `json:"schema_version"`
	Run           RunMeta  `json:"run"`
	Sources       []Source `json:"sources"`
	Stats         Stats    `json:"stats"`
	Hosts         []Host   `json:"hosts"`
	Warnings      []string `json:"warnings"`
}

// RunMeta is the report's own account of the run.
type RunMeta struct {
	ID                 string   `json:"id"`
	Domain             string   `json:"domain"`
	Input              string   `json:"input"` // domain or targets
	Scope              string   `json:"scope"`
	StartedAt          string   `json:"started_at"`
	FinishedAt         string   `json:"finished_at"`
	DurationMS         int64    `json:"duration_ms"`
	Completed          bool     `json:"completed"`
	TruncatedByTimeout bool     `json:"truncated_by_timeout"`
	Version            string   `json:"version"`
	Environment        string   `json:"environment"`
	Degraded           []string `json:"degraded"`
}

// Source is one enumeration source and how it fared.
type Source struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Found      int    `json:"found"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Stats are the report's counters.
type Stats struct {
	Enumerated   int `json:"enumerated"`
	Excluded     int `json:"excluded"`
	InScope      int `json:"in_scope"`
	Live         int `json:"live"`
	Dead         int `json:"dead"`
	Wildcard     int `json:"wildcard"`
	OpenPorts    int `json:"open_ports"`
	HTTPServices int `json:"http_services"`
}

// Host is one host and what each stage that reached it saw.
type Host struct {
	Host      string   `json:"host"`
	Status    string   `json:"status"` // discovered, live, dead, wildcard
	Reason    string   `json:"reason"` // nxdomain, no_answer, resolver_error
	Addresses []string `json:"addresses"`
	CNAME     []string `json:"cname"`
	Sources   []string `json:"sources"`
	CDN       []CDN    `json:"cdn"`
	Scan      *Scan    `json:"scan"`
	Ports     []Port   `json:"ports"`
}

// CDN is a CDN or WAF determination on some of the host's addresses.
type CDN struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Addresses   []string `json:"addresses"`
	ScanLimited bool     `json:"scan_limited"`
}

// Scan is what the port sweep attempted. Absent when the sweep did not run.
type Scan struct {
	Scanned  int `json:"scanned"`
	Open     int `json:"open"`
	Refused  int `json:"refused"`
	Filtered int `json:"filtered"`
	Unknown  int `json:"unknown"`
}

// Port is one open port.
type Port struct {
	Port      int      `json:"port"`
	Protocol  string   `json:"protocol"`
	State     string   `json:"state"`
	Addresses []string `json:"addresses"`
	HTTP      *HTTP    `json:"http"`
}

// HTTP is the probe's answer on a port.
type HTTP struct {
	URL                string   `json:"url"`
	FinalURL           string   `json:"final_url"`
	Scheme             string   `json:"scheme"`
	StatusCode         int      `json:"status_code"`
	Title              string   `json:"title"`
	ContentLength      int64    `json:"content_length"`
	ResponseTimeMS     int64    `json:"response_time_ms"`
	Server             string   `json:"server"`
	Redirects          []string `json:"redirects"`
	RedirectUnfollowed bool     `json:"redirect_unfollowed"`
	Tech               []string `json:"tech"` // ignored: technologies come from the Fingerprinter
	TLS                *TLS     `json:"tls"`
}

// TLS is the probed endpoint's certificate.
type TLS struct {
	SubjectCN    string   `json:"subject_cn"`
	Issuer       string   `json:"issuer"`
	NotAfter     string   `json:"not_after"`
	SANs         []string `json:"sans"`
	CertSPKIHash string   `json:"cert_spki_hash"`
}

// Parse decodes a report and refuses a schema major it was not written for.
func Parse(raw []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	if !strings.HasPrefix(r.SchemaVersion, "1.") {
		return nil, fmt.Errorf("report: schema_version %q is not 1.x", r.SchemaVersion)
	}
	return &r, nil
}

// Meta is what the run row keeps of a report: everything but the hosts.
func (r *Report) Meta() map[string]any {
	return map[string]any{
		"schema_version": r.SchemaVersion, "run": r.Run, "sources": r.Sources,
		"stats": r.Stats, "warnings": r.Warnings,
	}
}
