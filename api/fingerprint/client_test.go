package fingerprint

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Scan_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/scan" {
			t.Errorf("expected path /scan, got %s", r.URL.Path)
		}

		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if req.URL != "https://example.com" {
			t.Errorf("expected url 'https://example.com', got %q", req.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"url": "https://example.com",
			"chain": [{"url":"https://example.com","status_code":200,"title":"Example"}],
			"technologies": [{"name":"Nginx","version":"","category":"Server"}],
			"cookies": {},
			"metadata": {"robots_txt":true},
			"external_hosts": [],
			"scanned_at": "2026-03-14T10:00:00Z"
		}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	result, err := client.Scan("https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.URL != "https://example.com" {
		t.Errorf("expected URL 'https://example.com', got %q", result.URL)
	}
	if result.ScannedAt.IsZero() {
		t.Error("expected non-zero scanned_at")
	}
	if len(result.Chain) == 0 {
		t.Error("expected non-empty chain")
	}
	if len(result.Technologies) == 0 {
		t.Error("expected non-empty technologies")
	}
}

func TestClient_Scan_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"scan failed: no such host"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	result, err := client.Scan("https://invalid.example.com")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result != nil {
		t.Error("expected nil result on error")
	}
}

func TestClient_Scan_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	_, err := client.Scan("https://example.com")
	if err == nil {
		t.Fatal("expected error for 500 status")
	}
}

func TestClient_Scan_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	_, err := client.Scan("https://example.com")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestClient_Scan_ConnectionRefused(t *testing.T) {
	client := NewClient("http://127.0.0.1:1") // port 1 should refuse
	_, err := client.Scan("https://example.com")
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}
