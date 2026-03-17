package revalidation

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestExtractOpenPorts(t *testing.T) {
	ports := json.RawMessage(`{"tcp":{"22":{"web":null},"80":{"web":"http://example.com"},"443":{"web":"https://example.com"}},"udp":{}}`)

	result := extractOpenPorts(ports)
	sort.Strings(result)

	if len(result) != 3 {
		t.Fatalf("expected 3 ports, got %d: %v", len(result), result)
	}
	if result[0] != "22" || result[1] != "443" || result[2] != "80" {
		t.Errorf("unexpected ports: %v", result)
	}
}

func TestExtractOpenPorts_Empty(t *testing.T) {
	result := extractOpenPorts(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestExtractOpenPorts_InvalidJSON(t *testing.T) {
	result := extractOpenPorts(json.RawMessage(`invalid`))
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestTcpCheck_Refused(t *testing.T) {
	// Port 1 should refuse connections
	if tcpCheck("127.0.0.1", "1") {
		t.Error("expected false for connection refused")
	}
}
