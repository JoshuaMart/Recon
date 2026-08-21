package normalize_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/JoshuaMart/recon/internal/normalize"
)

func payload(t *testing.T, raw string) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return out
}

func normalized(t *testing.T, layer normalize.Layer, raw string) map[string]any {
	t.Helper()

	result, err := normalize.Payload(layer, payload(t, raw))
	if err != nil {
		t.Fatalf("normalize %s: %v", layer, err)
	}
	return result.Data
}

// The trap this whole package exists around: jsonb already sorts and
// deduplicates object keys on write, so deduplication appears to work on flat
// structures and fails exactly on the arrays, which are the fields carrying the
// volume.
func TestTwoAnswersListingTheSameThingsCompareEqual(t *testing.T) {
	t.Parallel()

	first := normalized(t, normalize.LayerDNS,
		`{"status":"live","addresses":["1.1.1.1","2.2.2.2"]}`)
	second := normalized(t, normalize.LayerDNS,
		`{"status":"live","addresses":["2.2.2.2","1.1.1.1"]}`)

	if !reflect.DeepEqual(first, second) {
		t.Errorf("the same answer in another order did not converge:\n%v\n%v", first, second)
	}
}

// The CNAME chain reads from the name to its target. Sorting it would claim a
// different chain, which is why it is not in the same sentence as addresses.
func TestTheCNAMEChainKeepsItsOrder(t *testing.T) {
	t.Parallel()

	out := normalized(t, normalize.LayerDNS,
		`{"status":"live","cname":["edge.example.net","origin.example.net"]}`)

	got := out["cname"].([]any)
	if got[0] != "edge.example.net" {
		t.Errorf("the chain was reordered: %v", got)
	}
}

func TestTheProbeSettingsBecomeCounts(t *testing.T) {
	t.Parallel()

	out := normalized(t, normalize.LayerTCP,
		`{"open_ports":[443,80],"scanned_ports":[80,443,8080],"closed_ports":[8080]}`)

	if _, present := out["scanned_ports"]; present {
		t.Error("a hundred identical port numbers stayed in the payload, once per asset")
	}
	if out["scanned_ports_count"] != 3 {
		t.Errorf("scanned_ports_count = %v, want 3: the count separates "+
			"'nothing else is open' from 'nothing else was tried'", out["scanned_ports_count"])
	}
	if ports := out["open_ports"].([]any); ports[0] != float64(80) {
		t.Errorf("open ports were not sorted: %v", ports)
	}
}

// Both measure the request that just happened, on the busiest layer of the
// system. A page carrying a session id would otherwise differ on every pass.
func TestTheHTTPLayerDropsWhatMeasuresTheRequest(t *testing.T) {
	t.Parallel()

	out := normalized(t, normalize.LayerHTTP,
		`{"scheme":"https","status_code":200,"response_time_ms":251,"content_length":1533}`)

	for _, name := range []string{"response_time_ms", "content_length"} {
		if _, present := out[name]; present {
			t.Errorf("%s survived, and it differs on every pass", name)
		}
	}
}

func TestTheFingerprintLayerDropsWhatItMustNotCarry(t *testing.T) {
	t.Parallel()

	out := normalized(t, normalize.LayerFingerprint, `{
		"url": "https://target.test/",
		"screenshot": "AAAA",
		"scanned_at": "2026-08-21T10:00:00Z",
		"version": "1.4.2",
		"network": {"host": "target.test", "ips": ["1.1.1.1"], "cname": "edge.net"},
		"chain": [{"url": "https://target.test/", "status_code": 200, "remote_ip_address": "1.1.1.1"}]
	}`)

	// The capture never reaches the database, and this is the barrier that
	// makes it an impossibility rather than an instruction.
	for _, name := range []string{"screenshot", "scanned_at", "version"} {
		if _, present := out[name]; present {
			t.Errorf("%s survived normalization", name)
		}
	}

	if network := out["network"].(map[string]any); network["ips"] != nil {
		t.Error("the renderer's own resolution survived: addresses belong to the DNS layer, " +
			"and a second producer for them cannot agree with the first on a fronted name")
	}
	if network := out["network"].(map[string]any); network["cname"] == nil {
		t.Error("the terminal CNAME was dropped too, and it is stable and attributes the CDN")
	}
	hop := out["chain"].([]any)[0].(map[string]any)
	if hop["remote_ip_address"] != nil {
		t.Error("the per-hop address survived")
	}
}

func TestCookieValuesGoAndNamesStay(t *testing.T) {
	t.Parallel()

	out := normalized(t, normalize.LayerFingerprint,
		`{"url":"https://target.test/","cookies":{"SESS_INTERNAL":"a3f9","csrftoken":"deadbeef"}}`)

	if _, present := out["cookies"]; present {
		t.Error("cookie values survived, and a session id is reissued on every scan")
	}
	names := out["cookie_names"].([]any)
	if len(names) != 2 || names[0] != "SESS_INTERNAL" {
		t.Errorf("cookie_names = %v, want the two names sorted", names)
	}
}

// A name generated per session cannot link any asset to any other, and it
// makes the payload differ on every pass. PHPSESSID is useless because it is
// everywhere, this because it is nowhere twice.
func TestARandomlyNamedCookieIsNotAPivot(t *testing.T) {
	t.Parallel()

	out := normalized(t, normalize.LayerFingerprint,
		`{"url":"https://target.test/","cookies":{"oc464pk3f400":"x","PHPSESSID":"y","wordpress_logged_in_9f":"z"}}`)

	names, _ := out["cookie_names"].([]any)
	for _, name := range names {
		if name == "oc464pk3f400" {
			t.Error("a per-session name was kept as a pivot")
		}
	}
	// The conservative half. A false positive here drops a real pivot in
	// silence, so a name somebody chose has to survive even when it is long
	// and carries digits.
	var kept int
	for _, name := range names {
		if name == "PHPSESSID" || name == "wordpress_logged_in_9f" {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("names somebody chose were dropped as random: %v", names)
	}
}

func TestHeadersGetTheirThreeTreatments(t *testing.T) {
	t.Parallel()

	out := normalized(t, normalize.LayerFingerprint, `{
		"url": "https://target.test/",
		"chain": [{"headers": {
			"Date": "Thu, 21 Aug 2026 10:00:00 GMT",
			"CF-RAY": "8f3a1c2d4e5f6789-CDG",
			"X-Response-Time-Ms": "42",
			"Vary": "Accept-Encoding, Origin",
			"Via": "1.1 edge, 1.1 origin",
			"Content-Security-Policy": "script-src 'self' 'nonce-r4nd0m'; img-src *",
			"Server": "nginx"
		}}]
	}`)

	headers := out["chain"].([]any)[0].(map[string]any)["headers"].(map[string]any)

	if _, present := headers["Date"]; present {
		t.Error("Date survived, and it measures the request rather than the service")
	}
	// The presence is evidence, the value is noise: dropping the header to be
	// rid of the value would throw away the CDN signature with it.
	if headers["CF-RAY"] != "" {
		t.Errorf("CF-RAY = %v, want an empty value with the name kept", headers["CF-RAY"])
	}
	// A suffix rule once missed exactly this name, and one chain grew on every
	// pass for that alone.
	if headers["X-Response-Time-Ms"] != "" {
		t.Errorf("X-Response-Time-Ms = %v, want emptied", headers["X-Response-Time-Ms"])
	}
	if headers["Vary"] != "Accept-Encoding, Origin" {
		t.Errorf("Vary = %v, want its values sorted", headers["Vary"])
	}
	// A hop chain is a sequence. Sorting it would claim another route.
	if headers["Via"] != "1.1 edge, 1.1 origin" {
		t.Errorf("Via was reordered: %v", headers["Via"])
	}
	// Replaced rather than removed: script-src 'self' 'nonce-' and
	// script-src 'self' are two different policies.
	if got := headers["Content-Security-Policy"]; got != "script-src 'self' 'nonce-'; img-src *" {
		t.Errorf("the CSP nonce was not replaced: %v", got)
	}
	if headers["Server"] != "nginx" {
		t.Errorf("Server was altered: %v", headers["Server"])
	}
}

// Strict rejection would make every release of the rendering service an
// ingestion outage. The counter is what keeps the alternative from being
// silence: a misspelt field shows up by name.
func TestAnUndeclaredFieldIsKeptAndCounted(t *testing.T) {
	t.Parallel()

	result, err := normalize.Payload(normalize.LayerHTTP,
		payload(t, `{"scheme":"https","status_code":200,"techs":["nginx"]}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if len(result.Unknown) != 1 || result.Unknown[0] != "techs" {
		t.Errorf("unknown = %v, want [techs]", result.Unknown)
	}
	if result.Data["techs"] == nil {
		t.Error("the field was dropped rather than kept: an unknown field is data somebody sent")
	}
}

func TestARequiredFieldIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := normalize.Payload(normalize.LayerHTTP, payload(t, `{"scheme":"https"}`)); err == nil {
		t.Error("a payload with no status code was accepted")
	}
}

// Normalizing an already normalized payload has to be a no-op, or the second
// pass would differ from the first and every observation would look changed.
func TestNormalizationIsIdempotentOnPayloads(t *testing.T) {
	t.Parallel()

	raw := `{"url":"https://target.test/","cookies":{"SESS":"x"},"external_hosts":["b.net","a.net"],
	         "chain":[{"headers":{"Vary":"b, a","CF-RAY":"abc"}}]}`

	once := normalized(t, normalize.LayerFingerprint, raw)
	encoded, err := json.Marshal(once)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	twice := normalized(t, normalize.LayerFingerprint, string(encoded))

	if !reflect.DeepEqual(once, twice) {
		t.Errorf("normalizing twice moved the payload:\n%v\n%v", once, twice)
	}
}
