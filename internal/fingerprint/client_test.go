package fingerprint

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The answers below are the Fingerprinter's own, as logged in production.
func TestScanTellsTheTargetFromTheRenderer(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		reason string // empty: the renderer's fault
	}{
		{500, `{"error":"scan failed: scan: navigating to https://auth.example.fr/: navigation failed: net::ERR_CERT_COMMON_NAME_INVALID"}`, "cert_common_name_invalid"},
		{500, `{"error":"scan failed: scan: navigating to http://ywh.example.fr/: navigation failed: net::ERR_CONNECTION_REFUSED"}`, "connection_refused"},
		{500, `{"error":"scan failed: scan: navigating to http://ywh.example.fr/: context deadline exceeded"}`, "timeout"},
		{500, `{"error":"scan failed: no responses received"}`, "no_response"},
		{408, `{"error":"scan timed out"}`, "timeout"},
		{500, `{"error":"browser pool exhausted"}`, ""},
		{502, `bad gateway`, ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, _, err := NewClient(srv.URL, time.Second).Scan(context.Background(), "https://x.example/")
		srv.Close()
		if got := errors.Is(err, ErrUnreachable); got != (tc.reason != "") {
			t.Errorf("%d %s: unreachable = %v (%v), want %v", tc.status, tc.body, got, err, tc.reason != "")
			continue
		}
		if tc.reason != "" && Reason(err) != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.body, Reason(err), tc.reason)
		}
	}
}
