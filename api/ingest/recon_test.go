package ingest

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestExtractWebURLs_WithWebServices(t *testing.T) {
	ports := json.RawMessage(`{
		"tcp": {
			"22":  {"web": null},
			"80":  {"web": "http://sub.example.com"},
			"443": {"web": "https://sub.example.com"}
		},
		"udp": {}
	}`)

	urls := extractWebURLs(ports)
	sort.Strings(urls)

	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d: %v", len(urls), urls)
	}
	if urls[0] != "http://sub.example.com" {
		t.Errorf("expected 'http://sub.example.com', got %q", urls[0])
	}
	if urls[1] != "https://sub.example.com" {
		t.Errorf("expected 'https://sub.example.com', got %q", urls[1])
	}
}

func TestExtractWebURLs_NoWebServices(t *testing.T) {
	ports := json.RawMessage(`{
		"tcp": {
			"22": {"web": null},
			"25": {"web": null}
		},
		"udp": {}
	}`)

	urls := extractWebURLs(ports)
	if len(urls) != 0 {
		t.Errorf("expected 0 URLs, got %d: %v", len(urls), urls)
	}
}

func TestExtractWebURLs_Empty(t *testing.T) {
	urls := extractWebURLs(nil)
	if urls != nil {
		t.Errorf("expected nil, got %v", urls)
	}
}

func TestExtractWebURLs_InvalidJSON(t *testing.T) {
	urls := extractWebURLs(json.RawMessage(`not json`))
	if urls != nil {
		t.Errorf("expected nil, got %v", urls)
	}
}

func TestHasWebService(t *testing.T) {
	withWeb := json.RawMessage(`{"tcp":{"443":{"web":"https://example.com"}},"udp":{}}`)
	withoutWeb := json.RawMessage(`{"tcp":{"22":{"web":null}},"udp":{}}`)

	if !hasWebService(withWeb) {
		t.Error("expected true for ports with web service")
	}
	if hasWebService(withoutWeb) {
		t.Error("expected false for ports without web service")
	}
	if hasWebService(nil) {
		t.Error("expected false for nil ports")
	}
}
