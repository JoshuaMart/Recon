package bbot_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/bbot"
)

func parseFile(t *testing.T, name string) bbot.Scan {
	t.Helper()
	file, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open the fixture: %v", err)
	}
	defer func() { _ = file.Close() }()

	scan, err := bbot.Parse(file)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return scan
}

// TestTheSliceOfARealScanDecodes reads events written by the tool rather than
// by this test. Every field shape below was a guess until a real file
// contradicted one of them.
func TestTheSliceOfARealScanDecodes(t *testing.T) {
	scan := parseFile(t, "scan.ndjson")

	if scan.Provenance.Name != "reference" {
		t.Errorf("scan name = %q, want the one the SCAN event carries", scan.Provenance.Name)
	}
	if got := scan.Provenance.Target; len(got) != 1 || got[0] != "acme-corp.test" {
		t.Errorf("target = %v, want the seed", got)
	}
	if scan.Provenance.Started.IsZero() {
		t.Error("started_at is zero, and the float epoch was supposed to convert")
	}
	if len(scan.Hosts) == 0 || len(scan.Services) == 0 {
		t.Fatalf("hosts = %d, services = %d, want both", len(scan.Hosts), len(scan.Services))
	}

	// The counts are the answer's only claim about the lines that became
	// nothing, so they have to sum to the totals reported beside them.
	var hosts, services int
	for _, count := range scan.Counts {
		hosts += count.Hosts
		services += count.Services
	}
	if hosts != len(scan.Hosts) {
		t.Errorf("per type hosts = %d, total = %d", hosts, len(scan.Hosts))
	}
	if services != len(scan.Services) {
		t.Errorf("per type services = %d, total = %d", services, len(scan.Services))
	}
}

// TestNoURLEventBecomesAnAsset is the rule of 4.3 seen from the producer side.
// The sample carries URL events with paths, and the only thing they may leave
// behind is the service under them.
func TestNoURLEventBecomesAnAsset(t *testing.T) {
	scan := parseFile(t, "scan.ndjson")

	for _, host := range scan.Hosts {
		if strings.Contains(host.Name, "/") || strings.HasPrefix(host.Name, "http") {
			t.Errorf("host %q is a URL, and no producer creates one", host.Name)
		}
	}
	count := scan.Counts[bbot.TypeURL]
	if count == nil || count.Seen == 0 {
		t.Fatal("the fixture carries URL events and the counts do not mention them")
	}
	if count.Note == "" {
		t.Error("a URL event produced no asset of its own and said nothing about why")
	}
}

// TestWhatIsReadForLineageCreatesNothing pins the other half: a protocol and a
// technology are measurements, and 7.6 keeps measurements out of the inventory.
func TestWhatIsReadForLineageCreatesNothing(t *testing.T) {
	scan := parseFile(t, "scan.ndjson")

	var banner, technology bool
	for _, service := range scan.Services {
		if service.Banner != "" && service.Protocol != "" {
			banner = true
		}
	}
	for _, host := range scan.Hosts {
		if len(host.Technologies) > 0 {
			technology = true
		}
	}
	if !banner {
		t.Error("no service carries the protocol and banner the fixture has")
	}
	if !technology {
		t.Error("no host carries a technology the fixture has")
	}

	for _, name := range []string{bbot.TypeASN, bbot.TypeOrgStub, bbot.TypeScan} {
		count := scan.Counts[name]
		if count == nil {
			t.Fatalf("%s is in the fixture and missing from the counts", name)
		}
		if count.Hosts != 0 || count.Services != 0 {
			t.Errorf("%s produced %d hosts and %d services, want none",
				name, count.Hosts, count.Services)
		}
		if count.Note == "" {
			t.Errorf("%s produced nothing and did not say why", name)
		}
	}
}

// TestABadLineCostsItselfAndNotTheFile is the discriminating case: the fixture
// puts a malformed line between two good ones, so a decoder that stops on the
// first error loses the third line and this test says which.
func TestABadLineCostsItselfAndNotTheFile(t *testing.T) {
	scan := parseFile(t, "edge.ndjson")

	names := map[string]bool{}
	for _, host := range scan.Hosts {
		names[host.Name] = true
	}
	for _, want := range []string{"good.example.com", "after.example.com", "orphan.example.com"} {
		if !names[want] {
			t.Errorf("%s is missing, so a bad line cost more than itself: %v", want, names)
		}
	}
	if names["broken.example.com"] {
		t.Error("the malformed line was decoded, which it cannot have been")
	}
	if len(scan.Refused) != 2 {
		t.Errorf("refused = %v, want the malformed line and the one with no type", scan.Refused)
	}
	if scan.Refused[0].Line != 2 {
		t.Errorf("the malformed line is line 2, reported as %d", scan.Refused[0].Line)
	}
}

// TestAnUnknownTypeIsCountedNeverRefused is the property the missing schema
// version forces. A release of the tool emitting something new must not be an
// outage here.
func TestAnUnknownTypeIsCountedNeverRefused(t *testing.T) {
	scan := parseFile(t, "edge.ndjson")

	count := scan.Counts["WEBSOCKET"]
	if count == nil {
		t.Fatalf("the unknown type is missing from the counts: %v", scan.Counts)
	}
	if count.Seen != 1 || count.Hosts != 0 || count.Services != 0 {
		t.Errorf("unknown type = %+v, want seen and nothing produced", count)
	}
	if count.Note == "" {
		t.Error("an unknown type was ignored silently")
	}
	for _, host := range scan.Hosts {
		if host.Name == "x.example.com" {
			t.Error("an unknown type created an asset")
		}
	}
}

// TestAJSONArrayIsRefusedByName is the shape jq produces, and the answer has to
// blame the container rather than the content.
func TestAJSONArrayIsRefusedByName(t *testing.T) {
	file, err := os.Open("testdata/array.json")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = file.Close() }()

	if _, err := bbot.Parse(file); err == nil {
		t.Fatal("a JSON array parsed, and the endpoint would import nothing while answering 200")
	} else if !strings.Contains(err.Error(), "newline delimited") {
		t.Errorf("refusal = %q, want it to name the shape it wanted", err)
	}
}

// TestAnEmptyBodyIsNotAnEmptyScan separates a file with nothing in it from a
// file that was not one. Both would otherwise import zero assets and answer 200.
func TestAnEmptyBodyIsNotAnEmptyScan(t *testing.T) {
	if _, err := bbot.Parse(strings.NewReader("\n  \n")); err == nil {
		t.Fatal("a body of whitespace parsed as a scan")
	}
}

// TestTheEarliestSightingIsTheOneKept is what makes the import back-date
// correctly. The file is not sorted by time, so a decoder taking the last
// sighting passes on sorted input and fails on a real one.
func TestTheEarliestSightingIsTheOneKept(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"DNS_NAME","id":"1","data":"a.example.com","timestamp":1785346500.0,"module":"late"}`,
		`{"type":"DNS_NAME","id":"2","data":"a.example.com","timestamp":1785346100.0,"module":"early"}`,
	}, "\n")

	scan, err := bbot.Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(scan.Hosts) != 1 {
		t.Fatalf("hosts = %d, want one name however many events carried it", len(scan.Hosts))
	}
	if want := time.Unix(1785346100, 0).UTC(); !scan.Hosts[0].At.Equal(want) {
		t.Errorf("at = %s, want the earlier sighting %s", scan.Hosts[0].At, want)
	}
	if scan.Hosts[0].Module != "early" {
		t.Errorf("module = %q, want the one that saw it first", scan.Hosts[0].Module)
	}
}

// TestANameIsLoweredAndItsTrailingDotRemoved keeps two spellings of one host
// from becoming two assets before normalize ever sees them.
func TestANameIsLoweredAndItsTrailingDotRemoved(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"DNS_NAME","id":"1","data":"API.Example.COM.","timestamp":1785346100.0}`,
		`{"type":"DNS_NAME","id":"2","data":"api.example.com","timestamp":1785346200.0}`,
	}, "\n")

	scan, err := bbot.Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(scan.Hosts) != 1 || scan.Hosts[0].Name != "api.example.com" {
		t.Errorf("hosts = %+v, want one canonical name", scan.Hosts)
	}
}

// TestAPortEventCreatesItsHost covers the file that carries a service whose
// DNS_NAME never reached the output. A service with no host is a service
// nothing schedules.
func TestAPortEventCreatesItsHost(t *testing.T) {
	scan := parseFile(t, "edge.ndjson")

	var found bool
	for _, host := range scan.Hosts {
		if host.Name == "orphan.example.com" {
			found = true
		}
	}
	if !found {
		t.Error("the port event's host was not created, so its service has no parent")
	}
	if len(scan.Services) != 1 || scan.Services[0].Port != 8443 {
		t.Errorf("services = %+v, want the one open port", scan.Services)
	}
}

// TestAnImpossiblePortCostsThePortAndNotTheHost. A port of zero is what a field
// absent from an event decodes to, and it must not become host:0/tcp. The host
// it named is still a host, and dropping it would lose a real name over a field
// that was missing.
func TestAnImpossiblePortCostsThePortAndNotTheHost(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"OPEN_TCP_PORT","id":"1","host":"a.example.com","port":0,"timestamp":1785346100.0}`,
		`{"type":"OPEN_TCP_PORT","id":"2","host":"b.example.com","port":70000,"timestamp":1785346100.0}`,
	}, "\n")

	scan, err := bbot.Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(scan.Services) != 0 {
		t.Errorf("services = %+v, want none", scan.Services)
	}
	if len(scan.Hosts) != 2 {
		t.Errorf("hosts = %+v, want both names the events carried", scan.Hosts)
	}
	// The per type numbers still have to sum to the totals beside them.
	count := scan.Counts[bbot.TypeOpenTCPPort]
	if count == nil || count.Hosts != 2 || count.Services != 0 {
		t.Errorf("counts = %+v, want two hosts and no service", count)
	}
}

// A body of rubbish must not become an answer bigger than the file. The refusal
// list is diagnostic, and the hundredth entry says what the first said.
func TestTheRefusalListIsBounded(t *testing.T) {
	var lines []string
	for range 500 {
		lines = append(lines, "{not json")
	}

	scan, err := bbot.Parse(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(scan.Refused) > 100 {
		t.Errorf("refused carries %d entries, and the bound is 100", len(scan.Refused))
	}
	// What was dropped is counted rather than hidden, which is the difference
	// between a bound and a silence.
	if scan.RefusedBeyond != 500-len(scan.Refused) {
		t.Errorf("refused beyond = %d, want the %d the list did not carry",
			scan.RefusedBeyond, 500-len(scan.Refused))
	}
}

// TestAURLWithNoPortStillNamesItsService. The producer usually computes the
// port and puts it at the top level, and an event where it did not would
// otherwise contribute nothing at all, silently.
func TestAURLWithNoPortStillNamesItsService(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"URL","id":"1","host":"a.example.com","data_json":{"url":"https://a.example.com/login"},"timestamp":1785346100.0}`,
		`{"type":"URL","id":"2","host":"b.example.com","data_json":{"url":"http://b.example.com/"},"timestamp":1785346100.0}`,
		`{"type":"URL","id":"3","host":"c.example.com","data_json":{"url":"https://c.example.com:8443/x"},"timestamp":1785346100.0}`,
	}, "\n")

	scan, err := bbot.Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	want := map[string]int{"a.example.com": 443, "b.example.com": 80, "c.example.com": 8443}
	if len(scan.Services) != len(want) {
		t.Fatalf("services = %+v, want one per URL", scan.Services)
	}
	for _, service := range scan.Services {
		if want[service.Host] != service.Port {
			t.Errorf("%s got port %d, want %d", service.Host, service.Port, want[service.Host])
		}
	}
	for _, host := range scan.Hosts {
		if strings.Contains(host.Name, "/") {
			t.Errorf("host %q is a path", host.Name)
		}
	}
}

// The top level port wins over the scheme's default, because the producer
// computed it and a redirect to a non standard port is exactly the case a
// scheme default gets wrong.
func TestTheTopLevelPortWinsOverTheSchemesDefault(t *testing.T) {
	scan, err := bbot.Parse(strings.NewReader(
		`{"type":"URL","id":"1","host":"a.example.com","port":8080,` +
			`"data_json":{"url":"https://a.example.com/"},"timestamp":1785346100.0}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(scan.Services) != 1 || scan.Services[0].Port != 8080 {
		t.Errorf("services = %+v, want the port the producer computed", scan.Services)
	}
}
