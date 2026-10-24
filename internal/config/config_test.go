package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
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
	if !reflect.DeepEqual(c, want) {
		t.Errorf("config.example.yml decodes to %+v, defaults are %+v", c, want)
	}
}

func TestPublicLeavesSecretsOut(t *testing.T) {
	c := Defaults()
	c.APIKey, c.Database.URL = "secret-key-1", "postgres://u:secret-db-2@db/recon"
	c.ScopesExtractor.APIKey, c.FastRecon.APIToken = "secret-se-3", "secret-fr-4"
	c.Notify.DiscordURL, c.Notify.WebhookURL = "https://discord.test/secret-dc-5", "https://hook.test/secret-wh-6"
	c.Notify.WebhookHeader, c.Runner.SecretKey = "X-Token: secret-wh-7", "secret-scw-8"
	b, err := json.Marshal(c.Public())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-") {
		t.Fatalf("a secret reached the settings page: %s", b)
	}
	if !strings.Contains(string(b), `"api_key":{"set":true}`) {
		t.Fatalf("whether the key is set is missing: %s", b)
	}
}

// The settings page shows every setting the example file documents: a new one
// cannot be left out.
func TestPublicListsEverySetting(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config.example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	example, err := yaml.Parser().Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var walk func(prefix string, want, got map[string]any)
	walk = func(prefix string, want, got map[string]any) {
		for k, v := range want {
			g, ok := got[k]
			if !ok {
				t.Errorf("%s%s is in config.example.yml but not on the settings page", prefix, k)
				continue
			}
			if wm, ok := v.(map[string]any); ok {
				if gm, ok := g.(map[string]any); ok && gm["set"] == nil {
					walk(prefix+k+".", wm, gm)
				}
			}
		}
	}
	walk("", example, Defaults().Public())
}

func TestRunnerValidation(t *testing.T) {
	valid := func(mod func(*Config)) error {
		c := Defaults()
		c.APIKey, c.Database.URL = key, "postgres://x"
		c.Runner.Provider, c.Runner.Region, c.Runner.JobID, c.Runner.SecretKey = "scaleway", "fr-par", "job", "secret"
		c.PublicURL = "https://recon.example"
		if mod != nil {
			mod(&c)
		}
		return c.Validate()
	}
	if err := valid(nil); err != nil {
		t.Fatalf("a full scaleway runner refused: %v", err)
	}
	for name, mod := range map[string]func(*Config){
		"no secret":            func(c *Config) { c.Runner.SecretKey = "" },
		"http callback":        func(c *Config) { c.Runner.CallbackURL = "http://vps.example:8080" },
		"http public url":      func(c *Config) { c.PublicURL = "http://recon.example" },
		"half set under serve": func(c *Config) { c.Runner.Provider, c.Runner.Region = "serve", "" },
		"unknown provider":     func(c *Config) { c.Runner.Provider = "lambda" },
	} {
		if valid(mod) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, mod := range map[string]func(*Config){
		"http to localhost": func(c *Config) { c.Runner.CallbackURL = "http://localhost:8080" },
		"serve, kept set":   func(c *Config) { c.Runner.Provider = "serve" },
		"serve, none set": func(c *Config) {
			c.Runner.Provider, c.Runner.Region, c.Runner.JobID, c.Runner.SecretKey = "serve", "", "", ""
		},
	} {
		if err := valid(mod); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}
