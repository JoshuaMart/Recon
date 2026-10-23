package fingerprint

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][]int32{
		"1.24.0":         {1, 24, 0},
		"v2.4":           {2, 4},
		"8.1.2-1ubuntu2": {8, 1, 2},
		"1.24":           {1, 24},
		"10":             {10},
		"latest":         nil,
		"":               nil,
	} {
		if got := ParseVersion(in); !slices.Equal(got, want) {
			t.Errorf("ParseVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPrecision(t *testing.T) {
	if !Precision("1.24", "1.24.0") || !Precision("1.24.3", "1.24") {
		t.Error("a prefix is a gain in precision")
	}
	if Precision("1.24", "1.25") || Precision("1.24.0", "1.24.1") || Precision("", "1.0") {
		t.Error("a different version is a change")
	}
}

func render(t *testing.T, s string) *Result {
	t.Helper()
	var r Result
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

// Two renders of the same page on two passes: everything that measures the
// request differs, nothing that describes the service does.
func TestNormalizeDropsWhatMeasuresTheRequest(t *testing.T) {
	a := render(t, `{"url":"https://a.example.com","chain":[{"url":"https://a.example.com/","status_code":200,"title":"Home",
		"headers":{"Date":"Mon","CF-RAY":"8a1b-CDG","CF-Cache-Status":"HIT","X-Amz-Cf-Pop":"CDG50-P1","Via":"1.1 a.cloudfront.net","Content-Security-Policy":"script-src 'self' 'nonce-abc123'","Server":"cloudflare","X-Request-Id":"1"},
		"response_size":1200,"remote_ip_address":"1.2.3.4"}],
		"technologies":[{"name":"PHP","version":"8.1","category":"Language"},{"name":"Cloudflare","category":"CDN"}],
		"cookies":{"PHPSESSID":"aaa","a1b2c3d4e5f6g7h8i9j0":"x"},
		"scripts":[{"url":"https://a.example.com/app.js","internal":true,"hash":"AB"}],
		"external_hosts":["cdn.example.net","api.example.net"],"web_sockets":[],
		"metadata":{"robots_txt":true,"llms_txt":false,"favicon_hash":"-123"},
		"screenshot":"AAAA","scanned_at":"2026-10-01T00:00:00Z","version":"v1.0.0"}`)
	b := render(t, `{"url":"https://a.example.com","chain":[{"url":"https://a.example.com/","status_code":200,"title":"Home",
		"headers":{"date":"Tue","cf-ray":"9z9z-AMS","cf-cache-status":"MISS","x-amz-cf-pop":"AMS1-C1","via":"1.1 b.cloudfront.net","content-security-policy":"script-src 'self' 'nonce-zzz999'","server":"cloudflare","x-request-id":"2"},
		"response_size":1500,"remote_ip_address":"5.6.7.8"}],
		"technologies":[{"name":"Cloudflare","category":"CDN"},{"name":"PHP","version":"8.1","category":"Language"}],
		"cookies":{"PHPSESSID":"bbb","z9y8x7w6v5u4t3s2r1q0":"y"},
		"scripts":[{"url":"https://a.example.com/app.js","internal":true,"hash":"ab"}],
		"external_hosts":["api.example.net","cdn.example.net"],"web_sockets":[],
		"metadata":{"robots_txt":true,"llms_txt":false,"favicon_hash":"-123"},
		"screenshot":"BBBB","scanned_at":"2026-10-08T00:00:00Z","version":"v1.1.0"}`)
	ja, _ := json.Marshal(Normalize(a))
	jb, _ := json.Marshal(Normalize(b))
	if string(ja) != string(jb) {
		t.Errorf("two renders of the same page differ:\n%s\n%s", ja, jb)
	}
	d := Normalize(a)
	h := d.Chain[0].Headers
	if _, ok := h["date"]; ok {
		t.Error("date was kept")
	}
	if v, ok := h["cf-ray"]; !ok || v != "" {
		t.Errorf("cf-ray must be kept, emptied: %q %v", v, ok)
	}
	if h["content-security-policy"] != "script-src 'self' 'nonce-'" {
		t.Errorf("csp = %q", h["content-security-policy"])
	}
	if !slices.Equal(d.CookieNames, []string{"PHPSESSID"}) {
		t.Errorf("cookie names = %v: values dropped, random names rejected", d.CookieNames)
	}
}

func TestMajor(t *testing.T) {
	for in, want := range map[string]int{"v1.2.3": 1, "2.0": 2, "10.4.1-rc1": 10} {
		if got, ok := Major(in); !ok || got != want {
			t.Errorf("Major(%q) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"b2d2e49", "main", "", "v1", "x.y"} {
		if _, ok := Major(in); ok {
			t.Errorf("Major(%q) parsed: a commit hash or a branch is not a version", in)
		}
	}
}
