// Package config loads the typed configuration: defaults in code, then a YAML
// file, then RECON_* environment variables. It is the only package that reads
// the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix prefixes every environment override. A double underscore nests:
// RECON_DATABASE__URL sets database.url.
const EnvPrefix = "RECON_"

// Config is the whole configuration. Sections are added by the phase that
// needs them.
type Config struct {
	// PublicURL is how runs and the browser reach Recon.
	PublicURL       string          `koanf:"public_url"`
	APIKey          string          `koanf:"api_key"`
	HTTP            HTTP            `koanf:"http"`
	Database        Database        `koanf:"database"`
	Log             Log             `koanf:"log"`
	ScopesExtractor ScopesExtractor `koanf:"scopes_extractor"`
	Discovery       Discovery       `koanf:"discovery"`
	FastRecon       FastRecon       `koanf:"fastrecon"`
	Verification    Verification    `koanf:"verification"`
}

// Verification is the periodic probing of known hosts.
type Verification struct {
	Enabled bool `koanf:"enabled"`
	// Batch is the most hosts one run carries.
	Batch   int           `koanf:"batch"`
	Resolve time.Duration `koanf:"resolve"` // nominal resolve cadence
	Full    time.Duration `koanf:"full"`    // nominal full cadence
	Dormant time.Duration `koanf:"dormant"` // cadence of inactive and unobservable hosts
	// ArchiveAfter is how long an inactive or unobservable asset waits before
	// it leaves the scheduler.
	ArchiveAfter time.Duration `koanf:"archive_after"`
	// DeadOrigins are the pages a CDN serves when the origin is gone.
	DeadOrigins []DeadOrigin `koanf:"dead_origins"`
}

// DeadOrigin is one dead-origin signature. Empty fields match anything.
type DeadOrigin struct {
	Name     string `koanf:"name"`
	Statuses []int  `koanf:"statuses"`
	Server   string `koanf:"server"`
	Title    string `koanf:"title"`
}

// FastRecon is the `fastrecon serve` instance runs are sent to. An empty URL
// leaves runs queued.
type FastRecon struct {
	URL      string `koanf:"url"`
	APIToken string `koanf:"api_token"`
	// Timeout is the run's own budget, sent with every request.
	Timeout time.Duration `koanf:"timeout"`
	// Slots is how many runs may be in flight: one per serve instance.
	Slots int `koanf:"slots"`
}

// ScopesExtractor is the scope sync. An empty URL disables it, leaving manual
// programs only.
type ScopesExtractor struct {
	URL      string        `koanf:"url"`
	APIKey   string        `koanf:"api_key"`
	Interval time.Duration `koanf:"interval"`
	Timeout  time.Duration `koanf:"timeout"`
	// MaxRemovedRatio is the share of active synced programs one pass may
	// archive before the pass is refused as suspicious.
	MaxRemovedRatio float64 `koanf:"max_removed_ratio"`
}

// Discovery holds the recon settings.
type Discovery struct {
	// AutoScan is the default for programs that do not set their own: off or on_new.
	AutoScan string `koanf:"auto_scan"`
	// Ports is the port list every run scans, recon and verification alike.
	Ports []int `koanf:"ports"`
}

// DefaultPorts are the ports that can carry a finding: web entry points, admin
// consoles, stores, remote access.
var DefaultPorts = []int{
	80, 443, 8080, 8443, 8000, 8090, 9000, 9443, 3000, 5000,
	2375, 2376, 6443, 10250, 15672, 5601, 8983, 9990, 2083, 2087,
	3306, 5432, 27017, 6379, 9200, 11211, 9042, 2379,
	3389, 5900, 1080, 3128,
}

// HTTP is the listener.
type HTTP struct {
	Addr string `koanf:"addr"`
}

// Database is the PostgreSQL connection.
type Database struct {
	URL string `koanf:"url"`
}

// Log is the logger output.
type Log struct {
	Level  string `koanf:"level"`  // debug, info, warn, error
	Format string `koanf:"format"` // json, text
}

// Defaults is the configuration before any file or variable.
func Defaults() Config {
	return Config{
		PublicURL: "http://localhost:8080",
		HTTP:      HTTP{Addr: ":8080"},
		Log:       Log{Level: "info", Format: "json"},
		ScopesExtractor: ScopesExtractor{
			Interval: time.Hour, Timeout: time.Minute, MaxRemovedRatio: 0.5,
		},
		Discovery: Discovery{AutoScan: "off", Ports: DefaultPorts},
		FastRecon: FastRecon{Timeout: 30 * time.Minute, Slots: 1},
		Verification: Verification{
			Enabled: true, Batch: 500, Resolve: 24 * time.Hour, Full: 72 * time.Hour,
			Dormant: 7 * 24 * time.Hour, ArchiveAfter: 30 * 24 * time.Hour,
			DeadOrigins: []DeadOrigin{{Name: "cloudflare origin down", Statuses: []int{521, 522, 523, 530}, Server: "cloudflare"}},
		},
	}
}

// Load merges the defaults, the file at path when it is not empty, and the
// environment, then validates the result.
func Load(path string) (Config, error) {
	return load(path, os.Environ())
}

func load(path string, environ []string) (Config, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return Config{}, fmt.Errorf("defaults: %w", err)
	}
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("config file %s: %w", path, err)
		}
	}
	envProvider := env.Provider(".", env.Opt{
		Prefix:        EnvPrefix,
		TransformFunc: envKey,
		EnvironFunc:   func() []string { return environ },
	})
	if err := k.Load(envProvider, nil); err != nil {
		return Config{}, fmt.Errorf("environment: %w", err)
	}

	var c Config
	if err := k.UnmarshalWithConf("", &c, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return Config{}, fmt.Errorf("decode: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// envKey maps RECON_DATABASE__URL to database.url. A single underscore stays
// part of the key, so RECON_PUBLIC_URL is public_url.
func envKey(k, v string) (string, any) {
	k = strings.ToLower(strings.TrimPrefix(k, EnvPrefix))
	return strings.ReplaceAll(k, "__", "."), v
}

// Validate reports every missing or malformed field at once.
func (c Config) Validate() error {
	var errs []error
	if c.APIKey == "" {
		errs = append(errs, errors.New("api_key is required"))
	} else if len(c.APIKey) < 32 {
		errs = append(errs, errors.New("api_key must be at least 32 characters"))
	}
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required"))
	}
	if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("public_url %q must be an absolute http or https URL", c.PublicURL))
	}
	if c.HTTP.Addr == "" {
		errs = append(errs, errors.New("http.addr is required"))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q is not one of debug, info, warn, error", c.Log.Level))
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format %q is not one of json, text", c.Log.Format))
	}
	if se := c.ScopesExtractor; se.URL != "" {
		if u, err := url.Parse(se.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("scopes_extractor.url %q must be an absolute http or https URL", se.URL))
		}
		if se.APIKey == "" {
			errs = append(errs, errors.New("scopes_extractor.api_key is required when scopes_extractor.url is set"))
		}
	}
	if c.ScopesExtractor.Interval < time.Minute {
		errs = append(errs, errors.New("scopes_extractor.interval must be at least 1m"))
	}
	if c.ScopesExtractor.Timeout <= 0 {
		errs = append(errs, errors.New("scopes_extractor.timeout must be positive"))
	}
	if r := c.ScopesExtractor.MaxRemovedRatio; r <= 0 || r > 1 {
		errs = append(errs, fmt.Errorf("scopes_extractor.max_removed_ratio %v must be in (0, 1]", r))
	}
	if f := c.FastRecon; f.URL != "" {
		if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("fastrecon.url %q must be an absolute http or https URL", f.URL))
		}
		if f.APIToken == "" {
			errs = append(errs, errors.New("fastrecon.api_token is required when fastrecon.url is set"))
		}
	}
	if c.FastRecon.Timeout < time.Minute {
		errs = append(errs, errors.New("fastrecon.timeout must be at least 1m"))
	}
	if c.FastRecon.Slots < 1 {
		errs = append(errs, errors.New("fastrecon.slots must be at least 1"))
	}
	if v := c.Verification; v.Batch < 1 || v.Resolve < time.Minute || v.Full < time.Minute || v.Dormant < time.Minute || v.ArchiveAfter < time.Hour {
		errs = append(errs, errors.New("verification: batch must be positive, cadences at least 1m, archive_after at least 1h"))
	}
	if len(c.Discovery.Ports) == 0 {
		errs = append(errs, errors.New("discovery.ports cannot be empty"))
	}
	for _, p := range c.Discovery.Ports {
		if p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("discovery.ports holds %d, not a port", p))
		}
	}
	if a := c.Discovery.AutoScan; a != "off" && a != "on_new" {
		errs = append(errs, fmt.Errorf("discovery.auto_scan %q is not off or on_new", a))
	}
	return errors.Join(errs...)
}
