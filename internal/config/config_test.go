package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const key = "0123456789abcdef0123456789abcdef"

func TestLoadMergesFileThenEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	yml := "api_key: " + key + "\ndatabase:\n  url: postgres://file\nlog:\n  level: debug\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := load(path, []string{
		"RECON_DATABASE__URL=postgres://env",
		"RECON_PUBLIC_URL=https://recon.example",
		"OTHER_VAR=ignored",
	})
	if err != nil {
		t.Fatal(err)
	}

	if c.Database.URL != "postgres://env" {
		t.Errorf("database.url = %q, the environment must win over the file", c.Database.URL)
	}
	if c.Log.Level != "debug" {
		t.Errorf("log.level = %q, the file must win over the defaults", c.Log.Level)
	}
	if c.Log.Format != "json" {
		t.Errorf("log.format = %q, the default must survive", c.Log.Format)
	}
	if c.PublicURL != "https://recon.example" {
		t.Errorf("public_url = %q, a single underscore must stay inside the key", c.PublicURL)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	_, err := load("", []string{"RECON_LOG__LEVEL=loud", "RECON_PUBLIC_URL=recon.example"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"api_key", "database.url", "public_url", "log.level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestScopesExtractorNeedsAKey(t *testing.T) {
	_, err := load("", []string{
		"RECON_API_KEY=" + key, "RECON_DATABASE__URL=postgres://x",
		"RECON_SCOPES_EXTRACTOR__URL=http://scopes:4567",
	})
	if err == nil || !strings.Contains(err.Error(), "scopes_extractor.api_key") {
		t.Fatalf("err = %v", err)
	}
}

func TestDurationsFromTheEnvironment(t *testing.T) {
	c, err := load("", []string{
		"RECON_API_KEY=" + key, "RECON_DATABASE__URL=postgres://x",
		"RECON_SCOPES_EXTRACTOR__INTERVAL=15m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ScopesExtractor.Interval.String() != "15m0s" {
		t.Errorf("interval = %v", c.ScopesExtractor.Interval)
	}
}

func TestShortAPIKeyIsRefused(t *testing.T) {
	_, err := load("", []string{"RECON_API_KEY=short", "RECON_DATABASE__URL=postgres://x"})
	if err == nil || !strings.Contains(err.Error(), "32 characters") {
		t.Fatalf("err = %v, want the length rule", err)
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	if _, err := load(filepath.Join(t.TempDir(), "absent.yml"), nil); err == nil {
		t.Fatal("a config path that does not exist must fail, not fall back to defaults")
	}
}

// The example file documents every setting with its default, so it must decode
// to exactly the defaults.
func TestExampleFileMatchesDefaults(t *testing.T) {
	c, err := load(filepath.Join("..", "..", "config.example.yml"), []string{
		"RECON_API_KEY=" + key, "RECON_DATABASE__URL=postgres://x",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.APIKey, want.Database.URL = key, "postgres://x"
	if c != want {
		t.Errorf("config.example.yml decodes to %+v, defaults are %+v", c, want)
	}
}
