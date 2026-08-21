package ingest_test

import (
	"encoding/json"
	"testing"

	"github.com/JoshuaMart/recon/internal/ingest"
)

// The report type is transcribed rather than shared, so the scanner can ship on
// its own cycle. The price of that decision is that a field can move inside the
// document without anything here failing to compile, and every test that builds
// this structure in Go is blind to it.
//
// So one test decodes a real document. This one is written against the
// scanner's own struct definitions at schema 1.1, and it is the only place a
// field's position in the document is asserted at all.
const document = `{
  "schema_version": "1.1",
  "run": {
    "id": "01J8Z",
    "domain": "acme.test",
    "input": "targets",
    "scope": "full",
    "stages": ["resolve", "portscan", "httpprobe"],
    "started_at": "2026-08-21T09:00:00Z",
    "finished_at": "2026-08-21T09:07:12Z",
    "duration_ms": 432000,
    "completed": true,
    "truncated_by_timeout": false,
    "version": "1.3.0",
    "environment": "serverless-job",
    "degraded": ["resolvers_unvalidated"]
  },
  "sources": [],
  "stats": {"enumerated": 0, "in_scope": 2, "live": 1, "dead": 1},
  "hosts": [
    {
      "host": "closed.acme.test",
      "status": "live",
      "addresses": ["93.184.216.34"],
      "scan": {"scanned": 32, "open": 0, "refused": 32, "filtered": 0, "unknown": 0}
    },
    {
      "host": "gone.acme.test",
      "status": "dead",
      "reason": "nxdomain",
      "cname": ["bucket.s3.example.net."]
    }
  ]
}`

func TestARealDocumentDecodesWhereTheCodeReads(t *testing.T) {
	t.Parallel()

	var report ingest.Report
	if err := json.Unmarshal([]byte(document), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The one that was wrong. degraded sits inside run, and reading it from
	// the top level compiles, passes every test built in Go, and silently lets
	// a run that could not vouch for its resolvers conclude a death.
	if !report.RanDegraded() {
		t.Fatal("a run carrying degraded codes did not read as degraded")
	}
	if report.Run.Degraded[0] != ingest.DegradedResolversUnvalidated {
		t.Fatalf("the code decoded as %q", report.Run.Degraded[0])
	}

	if report.Run.Input != "targets" {
		t.Errorf("run.input decoded as %q, and it is what says whether a missing host means anything", report.Run.Input)
	}

	scan := report.Hosts[0].Scan
	if scan == nil {
		t.Fatal("the sweep counts did not decode")
	}
	if scan.Scanned != 32 || scan.Refused != 32 {
		t.Fatalf("the counts decoded as %+v", *scan)
	}
	if !scan.Accounted() {
		t.Fatal("a sweep whose buckets cover it read as unaccounted")
	}

	// A host with no sweep at all is a different claim from one that swept
	// nothing, and the absent object is what carries it.
	if report.Hosts[1].Scan != nil {
		t.Error("a host the sweep never reached came back with counts")
	}
}
