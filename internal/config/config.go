// Package config loads the typed configuration: defaults in code, then a YAML
// file, then RECON_* environment variables. It is the only package that reads
// the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
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
	APIKey          string          `koanf:"api_key" secret:"true"`
	HTTP            HTTP            `koanf:"http"`
	Database        Database        `koanf:"database"`
	Log             Log             `koanf:"log"`
	ScopesExtractor ScopesExtractor `koanf:"scopes_extractor"`
	Discovery       Discovery       `koanf:"discovery"`
	FastRecon       FastRecon       `koanf:"fastrecon"`
	Verification    Verification    `koanf:"verification"`
	Render          Render          `koanf:"render"`
	Screenshots     Screenshots     `koanf:"screenshots"`
	Notify          Notify          `koanf:"notify"`
	CT              CT              `koanf:"ct"`
	Runner          Runner          `koanf:"runner"`
	// GeoIPDir holds GeoLite2-ASN.mmdb and GeoLite2-City.mmdb. Without them,
	// ASN and country stay empty.
	GeoIPDir string `koanf:"geoip_dir"`
}

// Render is the Fingerprinter and the render cadences. An empty URL leaves
// renders due.
type Render struct {
	URL string `koanf:"url"`
	// Concurrency is the most renders in flight, below the Fingerprinter's own
	// concurrent_scans: a 429 halves it, successes bring it back.
	Concurrency int           `koanf:"concurrency"`
	Timeout     time.Duration `koanf:"timeout"`  // one scan, the service's budget plus a margin
	Periodic    time.Duration `koanf:"periodic"` // next render after a successful one
	Retry       time.Duration `koanf:"retry"`    // next render after the target did not answer
}

// Screenshots is where and how screenshots are kept.
type Screenshots struct {
	Dir     string `koanf:"dir"`
	Quality int    `koanf:"quality"` // JPEG quality, 1 to 100
	Keep    int    `koanf:"keep"`    // distinct screenshots kept per asset
	// DiskThreshold is the share of the screenshot filesystem in use past
	// which a health notification fires.
	DiskThreshold float64 `koanf:"disk_threshold"`
}

// Runner is where recon and verification runs go. serve keeps them on
// fastrecon serve; scaleway starts a Serverless Job per run. CT candidates
// always stay on serve.
type Runner struct {
	Provider  string `koanf:"provider"` // serve or scaleway
	Endpoint  string `koanf:"endpoint"` // the Scaleway API
	Region    string `koanf:"region"`
	JobID     string `koanf:"job_id"` // the job definition, created once outside Recon
	SecretKey string `koanf:"secret_key" secret:"true"`
	// CallbackURL is where a job reaches Recon; public_url when empty.
	CallbackURL string `koanf:"callback_url"`
	Slots       int    `koanf:"slots"` // jobs in flight at once
	// Grace is how long past its deadline a run's report token stays valid.
	Grace time.Duration `koanf:"grace"`
	// ReportTimeout is the job's wait for Recon to take its report.
	ReportTimeout time.Duration `koanf:"report_timeout"`
}

// validate checks the runner. The Scaleway settings go together whenever one
// is set: with serve in effect they still follow the jobs started before.
func (r Runner) validate(callback string) []error {
	var errs []error
	if r.Provider != "serve" && r.Provider != "scaleway" {
		errs = append(errs, fmt.Errorf("runner.provider %q is not serve or scaleway", r.Provider))
	}
	if r.Provider != "scaleway" && r.JobID == "" && r.SecretKey == "" {
		return errs
	}
	if r.Region == "" || r.JobID == "" || r.SecretKey == "" {
		errs = append(errs, errors.New("runner: region, job_id and secret_key go together, and scaleway needs them"))
	}
	if u, err := url.Parse(r.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("runner.endpoint %q must be an absolute https URL", r.Endpoint))
	}
	// The run tokens travel with every call: never in clear across a network,
	// only to this machine.
	u, err := url.Parse(callback)
	secure := err == nil && u.Host != "" && (u.Scheme == "https" || (u.Scheme == "http" && loopback(u.Hostname())))
	if !secure {
		errs = append(errs, fmt.Errorf("the callback %q must be an https URL (http only to localhost)", callback))
	}
	if r.Slots < 1 || r.Grace < time.Minute || r.ReportTimeout < 30*time.Second {
		errs = append(errs, errors.New("runner: slots at least 1, grace at least 1m, report_timeout at least 30s"))
	}
	return errs
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CallbackBase is where a job reaches Recon.
func (c Config) CallbackBase() string {
	if c.Runner.CallbackURL != "" {
		return strings.TrimRight(c.Runner.CallbackURL, "/")
	}
	return strings.TrimRight(c.PublicURL, "/")
}

// CT is the Certificate Transparency feed. An empty URL leaves it off.
type CT struct {
	URL string `koanf:"url"` // the certstream websocket
	// MaxPerHour is how many names a program may gain from CT in an hour;
	// the overflow is counted, not created.
	MaxPerHour int           `koanf:"max_per_hour"`
	Reload     time.Duration `koanf:"reload"` // how often the apex set is read again
}

// Notify holds the channels notifications go to. With none, they are kept and
// listed on the settings page only.
type Notify struct {
	DiscordURL    string `koanf:"discord_url" secret:"true"`
	WebhookURL    string `koanf:"webhook_url" secret:"true"`    // such URLs commonly hold their token
	WebhookHeader string `koanf:"webhook_header" secret:"true"` // "Name: value", sent with each webhook call
	// BrokenAfter is how long a component fails before it is announced.
	BrokenAfter time.Duration `koanf:"broken_after"`
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
	APIToken string `koanf:"api_token" secret:"true"`
	// Timeout is the run's own budget, sent with every request.
	Timeout time.Duration `koanf:"timeout"`
	// Slots is how many runs may be in flight: one per serve instance.
	Slots int `koanf:"slots"`
}

// ScopesExtractor is the scope sync. An empty URL disables it, leaving manual
// programs only.
type ScopesExtractor struct {
	URL      string        `koanf:"url"`
	APIKey   string        `koanf:"api_key" secret:"true"`
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
	URL string `koanf:"url" secret:"true"` // holds the password
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
		Render: Render{
			Concurrency: 20, Timeout: 2 * time.Minute, Periodic: 21 * 24 * time.Hour, Retry: 6 * time.Hour,
		},
		Screenshots: Screenshots{Dir: "/data/screenshots", Quality: 70, Keep: 3, DiskThreshold: 0.9},
		Notify:      Notify{BrokenAfter: 5 * time.Minute},
		CT:          CT{MaxPerHour: 40, Reload: 5 * time.Minute},
		Runner: Runner{
			Provider: "serve", Endpoint: "https://api.scaleway.com", Slots: 4,
			Grace: 30 * time.Minute, ReportTimeout: 5 * time.Minute,
		},
		GeoIPDir: "/data/geoip",
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
	if r := c.Render; r.URL != "" {
		if u, err := url.Parse(r.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("render.url %q must be an absolute http or https URL", r.URL))
		}
	}
	if r := c.Render; r.Concurrency < 1 || r.Timeout < time.Second || r.Periodic < time.Hour || r.Retry < time.Minute {
		errs = append(errs, errors.New("render: concurrency at least 1, timeout at least 1s, periodic at least 1h, retry at least 1m"))
	}
	if s := c.Screenshots; s.Dir == "" || s.Quality < 1 || s.Quality > 100 || s.Keep < 1 || s.DiskThreshold <= 0 || s.DiskThreshold > 1 {
		errs = append(errs, errors.New("screenshots: dir required, quality 1 to 100, keep at least 1, disk_threshold above 0 and at most 1"))
	}
	for name, v := range map[string]string{"notify.discord_url": c.Notify.DiscordURL, "notify.webhook_url": c.Notify.WebhookURL} {
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			// The value may hold a token: name the setting, never print it.
			errs = append(errs, fmt.Errorf("%s must be an absolute http or https URL", name))
		}
	}
	if h := c.Notify.WebhookHeader; h != "" {
		if name, value, ok := strings.Cut(h, ":"); !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
			errs = append(errs, errors.New("notify.webhook_header must be in 'Name: value' form"))
		}
	}
	if u := c.CT.URL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "ws" && p.Scheme != "wss") || p.Host == "" {
			errs = append(errs, fmt.Errorf("ct.url %q must be an absolute ws or wss URL", u))
		}
	}
	if c.CT.MaxPerHour < 1 || c.CT.Reload < time.Minute {
		errs = append(errs, errors.New("ct: max_per_hour at least 1, reload at least 1m"))
	}
	errs = append(errs, c.Runner.validate(c.CallbackBase())...)
	if c.Notify.BrokenAfter < time.Minute {
		errs = append(errs, errors.New("notify.broken_after must be at least 1m"))
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

// Public is the config in effect for the settings page, keyed like the config
// file. A field tagged secret shows only whether it is set.
func (c Config) Public() map[string]any {
	return public(reflect.ValueOf(c)).(map[string]any)
}

var durationType = reflect.TypeFor[time.Duration]()

func public(v reflect.Value) any {
	switch {
	case v.Type() == durationType:
		return time.Duration(v.Int()).String()
	case v.Kind() == reflect.Struct:
		out := map[string]any{}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			name := f.Tag.Get("koanf")
			if name == "" || !f.IsExported() {
				continue
			}
			if f.Tag.Get("secret") == "true" {
				out[name] = map[string]bool{"set": !v.Field(i).IsZero()}
				continue
			}
			out[name] = public(v.Field(i))
		}
		return out
	case v.Kind() == reflect.Slice:
		out := make([]any, v.Len())
		for i := range v.Len() {
			out[i] = public(v.Index(i))
		}
		return out
	}
	return v.Interface()
}
