package notify

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFormatTechnologies(t *testing.T) {
	raw := json.RawMessage(`[
		{"name":"PHP","version":"8.2.3","category":"Language"},
		{"name":"Nginx","version":"","category":"Server"}
	]`)

	names, techs := FormatTechnologies(raw)

	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
	if names[0] != "PHP 8.2.3" {
		t.Errorf("expected 'PHP 8.2.3', got %q", names[0])
	}
	if names[1] != "Nginx" {
		t.Errorf("expected 'Nginx', got %q", names[1])
	}
	if len(techs) != 2 {
		t.Fatalf("expected 2 techs, got %d", len(techs))
	}
	if techs[0].Category != "Language" {
		t.Errorf("expected category 'Language', got %q", techs[0].Category)
	}
}

func TestFormatTechnologies_Empty(t *testing.T) {
	names, techs := FormatTechnologies(nil)
	if names != nil || techs != nil {
		t.Error("expected nil for empty input")
	}
}

func TestFormatTechnologies_Invalid(t *testing.T) {
	names, techs := FormatTechnologies(json.RawMessage(`not json`))
	if names != nil || techs != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestExtractFavicon(t *testing.T) {
	raw := json.RawMessage(`{"robots_txt":true,"sitemap":"https://example.com/sitemap.xml","favicon":"https://example.com/favicon.ico"}`)
	favicon := ExtractFavicon(raw)
	if favicon != "https://example.com/favicon.ico" {
		t.Errorf("expected favicon URL, got %q", favicon)
	}
}

func TestExtractFavicon_Empty(t *testing.T) {
	if favicon := ExtractFavicon(nil); favicon != "" {
		t.Errorf("expected empty, got %q", favicon)
	}
}

func TestExtractFavicon_NoFavicon(t *testing.T) {
	raw := json.RawMessage(`{"robots_txt":true}`)
	if favicon := ExtractFavicon(raw); favicon != "" {
		t.Errorf("expected empty, got %q", favicon)
	}
}

func TestExtractChainInfo(t *testing.T) {
	raw := json.RawMessage(`[{"url":"https://example.com","status_code":200,"title":"Example Domain"}]`)
	code, title := ExtractChainInfo(raw)
	if code != 200 {
		t.Errorf("expected 200, got %d", code)
	}
	if title != "Example Domain" {
		t.Errorf("expected 'Example Domain', got %q", title)
	}
}

func TestExtractChainInfo_Empty(t *testing.T) {
	code, title := ExtractChainInfo(nil)
	if code != 0 || title != "" {
		t.Errorf("expected 0 and empty, got %d %q", code, title)
	}
}

func TestExtractChainInfo_EmptyArray(t *testing.T) {
	code, title := ExtractChainInfo(json.RawMessage(`[]`))
	if code != 0 || title != "" {
		t.Errorf("expected 0 and empty, got %d %q", code, title)
	}
}

func TestBuildWebServiceNotification(t *testing.T) {
	chain := json.RawMessage(`[{"status_code":301,"title":"Redirect"},{"status_code":200,"title":"Home"}]`)
	techs := json.RawMessage(`[{"name":"Laravel","version":"10.0","category":"Framework"}]`)
	metadata := json.RawMessage(`{"favicon":"https://example.com/fav.ico"}`)

	discord, callback := BuildWebServiceNotification(
		"https://sub.example.com", "*.example.com", "uuid-123",
		chain, techs, metadata,
	)

	// Discord uses first chain entry
	if discord.StatusCode != 301 {
		t.Errorf("expected status 301, got %d", discord.StatusCode)
	}
	if discord.Title != "Redirect" {
		t.Errorf("expected 'Redirect', got %q", discord.Title)
	}
	if discord.Wildcard != "*.example.com" {
		t.Errorf("expected '*.example.com', got %q", discord.Wildcard)
	}
	if len(discord.Technologies) != 1 || discord.Technologies[0] != "Laravel 10.0" {
		t.Errorf("unexpected technologies: %v", discord.Technologies)
	}

	// Callback
	if callback.Event != "" {
		t.Errorf("event should be empty before sending, got %q", callback.Event)
	}
	if callback.HostnameID != "uuid-123" {
		t.Errorf("expected 'uuid-123', got %q", callback.HostnameID)
	}
	if callback.Fingerprint.Favicon != "https://example.com/fav.ico" {
		t.Errorf("expected favicon, got %q", callback.Fingerprint.Favicon)
	}
	if len(callback.Fingerprint.Technologies) != 1 {
		t.Fatalf("expected 1 tech, got %d", len(callback.Fingerprint.Technologies))
	}
	if callback.Fingerprint.Technologies[0].Name != "Laravel" {
		t.Errorf("expected 'Laravel', got %q", callback.Fingerprint.Technologies[0].Name)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds int
		want    string
	}{
		{0, "0m00s"},
		{65, "1m05s"},
		{872, "14m32s"},
	}
	for _, tt := range tests {
		got := formatDuration(time.Duration(tt.seconds) * time.Second)
		if got != tt.want {
			t.Errorf("formatDuration(%ds) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}
