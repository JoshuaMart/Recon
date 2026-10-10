// Package config is the only place in this repository that reads the
// environment, enforced by the linter. Everything else receives a *Config.
//
// Merge order: defaults in code, a YAML file, then RECON_ environment
// variables. An option's env name is its path uppercased with "." as "__", so
// database.migration_url is RECON_DATABASE__MIGRATION_URL. Two underscores
// nest; one stays part of a key.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/JoshuaMart/recon/internal/notify"
)

// Role is which binary is asking. It decides what a configuration must carry
// and must not: database credential separation is checked here rather than left
// to the next compose file.
type Role string

const (
	// RoleControlPlane serves the API and runs the background loops.
	RoleControlPlane Role = "controlplane"
	// RoleMigrate applies and rolls back migrations, as the owner.
	RoleMigrate Role = "migrate"
)

// MinSigningKeyLength mirrors what the signer enforces, so a bad key fails at
// startup and not on the first authenticated run.
const MinSigningKeyLength = 32

// Runners a deployment can start runs with.
const (
	// RunnerNone renders the definition and starts nothing.
	RunnerNone = "none"
	// RunnerScaleway starts a serverless job definition.
	RunnerScaleway = "scaleway"
)

// Environments the configuration accepts. Several options are refused outside
// dev, so an unknown value must fail instead of defaulting to the permissive side.
const (
	EnvDev     = "dev"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// Config is the whole configuration, typed and validated once at startup.
type Config struct {
	Env      string   `koanf:"env"`
	Log      Log      `koanf:"log"`
	HTTP     HTTP     `koanf:"http"`
	Database Database `koanf:"database"`
	Enrich   Enrich   `koanf:"enrich"`
	Security Security `koanf:"security"`

	Maintenance  Maintenance  `koanf:"maintenance"`
	External     External     `koanf:"external"`
	Verification Verification `koanf:"verification"`
	Render       Render       `koanf:"render"`
	Runner       Runner       `koanf:"runner"`
	Notify       Notify       `koanf:"notify"`
	CT           CT           `koanf:"ct"`
}

// CT is the Certificate Transparency feed and its matcher. The feed is
// certstream-server-go, deployed as an image; this process only dials it, so
// the only address here is the aggregator's.
type CT struct {
	// URL is the aggregator's websocket. Empty turns the matcher off, a normal
	// deployment: periodic enumeration still walks every perimeter.
	URL string `koanf:"url"`
	// Interval reloads the apex set and flushes the counters. It also sets the
	// granularity of the feed's presence record, coarser than its table above a minute.
	Interval time.Duration `koanf:"interval"`
	// Ceiling is how many candidates one programme may create per Window.
	//
	// It bounds a mistake: an apex rule pointing at someone else's shared
	// infrastructure is one typo away, and a public feed would decide the size of an
	// inventory. What was held back is logged when the window rolls, since a silent
	// cap reads as a small answer.
	Ceiling int           `koanf:"ceiling"`
	Window  time.Duration `koanf:"window"`
	// CacheTTL and CacheSize bound short-term deduplication. It is a cost control;
	// correctness is the database's unique constraint, so both may err either way.
	CacheTTL  time.Duration `koanf:"cache_ttl"`
	CacheSize int           `koanf:"cache_size"`
}

// Notify is where alerts go and how often they leave. One generic webhook:
// Discord and Slack are a payload template, not connectors.
type Notify struct {
	// WebhookURL is the configured channel. Empty is a deliberate setup, not an
	// outage: events are marked delivered and counted, so "computed and sent
	// nowhere" stays visible.
	WebhookURL string `koanf:"webhook_url"`
	// Template renders the body. Empty takes the default, accepted by Discord and Slack.
	Template string `koanf:"template"`
	// MinPriority is the floor the configured channel wants.
	MinPriority string `koanf:"min_priority"`
	// Interval is how often the queue is drained, which is what "immediate" means:
	// a takeover waits at most one tick.
	Interval time.Duration `koanf:"interval"`
	Batch    int           `koanf:"batch"`
	Timeout  time.Duration `koanf:"timeout"`
	// UnobservableAlert is the share of a programme's inventory that makes a mass
	// tip an event.
	UnobservableAlert float64 `koanf:"unobservable_alert"`
}

// Runner is how a run definition is started.
//
// The control plane starts, it never updates: the update call replaces the
// whole environment map, wiping the source API keys the definition carries
// without any error. Only the start call is made, with per-run overrides.
type Runner struct {
	// Provider is "none" or "scaleway". None renders the definition and starts
	// nothing: the development shape, where a person runs the image with what the
	// console shows. It is production minus the call, so the local path is not a
	// second way of starting a run.
	Provider string `koanf:"provider"`
	Region   string `koanf:"region"`
	// JobID names the definition to start; it is deployed once, out of band.
	JobID string `koanf:"job_id"`
	// SecretKey is the only credential, scoped to starting this definition. It sits
	// in the process holding the inventory, so its narrowness bounds the damage of
	// a compromise.
	SecretKey string `koanf:"secret_key"`
	// Endpoint is the API base, so tests do not call a cloud.
	Endpoint string        `koanf:"endpoint"`
	Timeout  time.Duration `koanf:"timeout"`
}

// Render is the browser side of verification. It is separate from verification
// because a probe takes milliseconds while a render takes seconds and hundreds
// of megabytes, so they cannot share a batch size or cadence.
type Render struct {
	// URL is where the rendering service listens. Empty turns the loop off: assets
	// keep their due dates and nothing pretends to have looked.
	URL     string        `koanf:"url"`
	Timeout time.Duration `koanf:"timeout"`
	// Interval is how often the pass runs.
	Interval time.Duration `koanf:"interval"`
	Batch    int           `koanf:"batch"`
	// Concurrency is set above where the budget binds, so the throttle is the
	// programme's published rate limit rather than an uncalibrated number.
	Concurrency int `koanf:"concurrency"`
	// Cost is what one render is charged against a programme's rate limit. A
	// browser fetches the page and everything it pulls, so billing it as one request
	// would make the costliest operation the cheapest.
	Cost int `koanf:"cost"`
	// UnobservableAlert is the share of a programme's inventory that turns a mass
	// tip into an alert; a mass tip is about the observer, not the targets.
	UnobservableAlert float64 `koanf:"unobservable_alert"`
	// ReplanSpread is how long a forced refresh after a service update is spread
	// over. It restores baselines without a mass alert, which an hour would be.
	ReplanSpread time.Duration `koanf:"replan_spread"`
}

// Verification is the loop that keeps the inventory honest and the shape of its
// runs. Cadences are configuration because the stage ladder is the cost knob: a
// resolve is one resolver round trip, a full is a hundred connections per host.
type Verification struct {
	Resolve     time.Duration `koanf:"resolve"`
	Full        time.Duration `koanf:"full"`
	Fingerprint time.Duration `koanf:"fingerprint"`
	Inactive    time.Duration `koanf:"inactive"`
	Jitter      time.Duration `koanf:"jitter"`
	// RenderSole, RenderRecovery and RenderBlind are the cadences of the three
	// non-nominal regimes: who is still detecting, and whether a render is a
	// measurement or a recovery attempt.
	RenderSole     time.Duration `koanf:"render_sole"`
	RenderRecovery time.Duration `koanf:"render_recovery"`
	RenderBlind    time.Duration `koanf:"render_blind"`
	// FullFloor is the fastest a failing asset's port sweep may run. A backoff curve
	// is written for the cheap rung; unchanged on the expensive one it becomes a flood.
	FullFloor time.Duration `koanf:"full_floor"`
	// BatchSize is how many hosts one run freezes. A due date decides eligibility,
	// this decides what goes out.
	BatchSize int `koanf:"batch_size"`
	// Timeout is a run's own budget and must expire before the platform's, which
	// sits on a job definition Recon does not control. A run killed from outside
	// delivers nothing; one that runs out of its own time delivers a truncated report.
	Timeout time.Duration `koanf:"timeout"`
	// CandidateTimeout is a candidate run's budget, much shorter since it does one
	// rung over a short list. The deadline is how long the lane's slot is held.
	CandidateTimeout time.Duration `koanf:"candidate_timeout"`
	// Grace is how long past the deadline a report is still ingested. The data is
	// valid; the run may have been re-dispatched.
	Grace time.Duration `koanf:"grace"`
	// ReportTimeout is how long the scanner waits for Recon to answer a report and
	// how long Recon gives itself to write one. FastRecon's thirty second default
	// is shorter than a large discovery report takes: the scanner hung up, the
	// write rolled back, and retries repeated the same work.
	ReportTimeout time.Duration `koanf:"report_timeout"`
	// SweepInterval is how often runs past their deadline are expired; an
	// unexpired run holds its targets forever.
	SweepInterval time.Duration `koanf:"sweep_interval"`
	// DiscoveryRetry is how long after an empty discovery run a replacement may be
	// provisioned. Far shorter than the discovery interval, so a thirty-second
	// failure does not cost a week, and far longer than the tick, so a permanently
	// broken runner is not provisioned and billed every tick.
	DiscoveryRetry time.Duration `koanf:"discovery_retry"`
	// Ports is the curated list, as data. It travels in the run definition so
	// discovery and verification scan the same ports; a second list on the scanner
	// could diverge without any error.
	Ports string `koanf:"ports"`
	// PublicURL is the base a run reaches this control plane on. It cannot be
	// derived from the listen address: the scanner runs elsewhere.
	PublicURL string `koanf:"public_url"`
}

// Security holds the one secret the control plane signs with. Run credentials
// are HMACs over run, purpose and expiry, so nothing is stored or revoked. All
// of it rests on this key, hence a short one is refused: a guessable key lets
// anyone mint a token for any run.
type Security struct {
	SigningKey string `koanf:"signing_key"`
}

// Maintenance is the housekeeping tick. It has no enable flag: a partition job
// that can be turned off is an ingestion outage three months later.
type Maintenance struct {
	Interval time.Duration `koanf:"interval"`
}

// Log controls what the process writes to stderr. Everything is structured and
// carries a correlation id; adding that later touches every write path.
type Log struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

// HTTP is the control plane's listener.
type HTTP struct {
	Addr            string        `koanf:"addr"`
	ReadTimeout     time.Duration `koanf:"read_timeout"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
}

// Database holds one connection string per role, as separate variables: the
// role is chosen when a pool is opened, not case by case in the code.
type Database struct {
	// URL is asm_app: the control plane's requests, under row-level security.
	URL string `koanf:"url"`
	// SystemURL is asm_sys: the background loops serving every tenant in one tick,
	// and the two lookups turning a credential into an organization. Required on the
	// control plane, since under row-level security a loop on the application role
	// reads zero rows instead of failing.
	SystemURL string `koanf:"system_url"`
	// MigrationURL is asm_owner. It must never be in the control plane's
	// environment, or role separation buys nothing.
	MigrationURL string `koanf:"migration_url"`

	MaxConns int32 `koanf:"max_conns"`
	// SystemMaxConns bounds the second pool separately. Sharing one setting would
	// make a deployment tuned for N backends quietly open 2N, which on a managed
	// instance with a hard max_connections breaks the next service. It is much
	// smaller because the work is: a few timed loops against a request-serving pool.
	SystemMaxConns int32         `koanf:"system_max_conns"`
	ConnectTimeout time.Duration `koanf:"connect_timeout"`
}

// External is the sweep asking whether someone else's domain is still
// registered.
//
// A DNS lookup is allowed outside a scan authorization: the guardrails protect
// interaction with a third party's infrastructure, and a public resolver query
// reaches neither the domain nor its servers. An interval of zero turns the
// loop off for deployments that want no outbound queries.
type External struct {
	Interval time.Duration `koanf:"interval"`
	Timeout  time.Duration `koanf:"timeout"`
	Batch    int           `koanf:"batch"`
}

// Enrich points at the Geo-IP databases. Both paths are optional, but the
// difference must be visible: the console cannot tell "not configured" from
// "configured, no match", as both leave an asset without an operator.
type Enrich struct {
	CityDatabase string `koanf:"city_database"`
	ASNDatabase  string `koanf:"asn_database"`
}

// Defaults are the values a deployment inherits when it says nothing. They live
// in code so an empty environment still works.
func Defaults() Config {
	return Config{
		Env: EnvDev,
		Log: Log{Level: "info", Format: "json"},
		HTTP: HTTP{
			Addr:            ":8080",
			ReadTimeout:     10 * time.Second,
			ShutdownTimeout: 15 * time.Second,
		},
		Database: Database{
			MaxConns:       10,
			SystemMaxConns: 4,
			ConnectTimeout: 5 * time.Second,
		},
		Maintenance: Maintenance{Interval: time.Hour},
		// Six hours: an expiring domain is a step that stays, so a tighter cadence buys
		// latency on a fact that does not move at the cost of queries against every
		// referenced third party.
		External: External{Interval: 6 * time.Hour, Timeout: 5 * time.Second, Batch: 5000},
		Notify: Notify{
			MinPriority:       "low",
			Interval:          30 * time.Second,
			Batch:             200,
			Timeout:           10 * time.Second,
			UnobservableAlert: 0.10,
		},
		Runner: Runner{
			Provider: RunnerNone,
			Region:   "fr-par",
			Endpoint: "https://api.scaleway.com",
			Timeout:  30 * time.Second,
		},
		CT: CT{
			Interval:  time.Minute,
			Ceiling:   500,
			Window:    time.Hour,
			CacheTTL:  10 * time.Minute,
			CacheSize: 50_000,
		},
		Render: Render{
			Timeout:           2 * time.Minute,
			Interval:          time.Minute,
			Batch:             200,
			Concurrency:       8,
			Cost:              30,
			UnobservableAlert: 0.2,
			ReplanSpread:      3 * 24 * time.Hour,
		},
		Verification: Verification{
			Resolve:          24 * time.Hour,
			Full:             72 * time.Hour,
			Fingerprint:      21 * 24 * time.Hour,
			RenderSole:       7 * 24 * time.Hour,
			RenderRecovery:   30 * 24 * time.Hour,
			RenderBlind:      7 * 24 * time.Hour,
			Inactive:         7 * 24 * time.Hour,
			Jitter:           15 * time.Minute,
			FullFloor:        6 * time.Hour,
			BatchSize:        500,
			Timeout:          30 * time.Minute,
			CandidateTimeout: 5 * time.Minute,
			Grace:            10 * time.Minute,
			ReportTimeout:    10 * time.Minute,
			SweepInterval:    time.Minute,
			DiscoveryRetry:   time.Hour,
			// Not nmap's top 100, which ranks by how often a port is open across the
			// internet and so leads with mail and printing. A port earns its place if it
			// can carry a finding.
			Ports: "80,443,8080,8443,8000,8090,9000,9443,3000,5000," +
				"2375,2376,6443,10250,15672,5601,8983,9990,2083,2087," +
				"3306,5432,27017,6379,9200,11211,9042,2379," +
				"3389,5900,1080,3128",
		},
	}
}

// Options adjust where a configuration is read from. The zero value reads the
// process environment and the file RECON_CONFIG names, as every binary does.
type Options struct {
	// File is an optional YAML path, overriding RECON_CONFIG. Missing is not an
	// error: environment-only deployments are normal.
	File string
	// Environ overrides the source of environment variables, so tests never touch
	// the process environment and can run in parallel.
	Environ func() []string
}

// configVar names the file to read. It is not a structure option: a
// configuration cannot say where it is loaded from.
const configVar = prefix + "CONFIG"

func (o Options) environ() []string {
	if o.Environ != nil {
		return o.Environ()
	}
	return os.Environ()
}

func (o Options) path() string {
	if o.File != "" {
		return o.File
	}
	for _, entry := range o.environ() {
		if value, ok := strings.CutPrefix(entry, configVar+"="); ok {
			return value
		}
	}
	return ""
}

const (
	prefix = "RECON_"
	delim  = "."
	nest   = "__"
)

// Load merges the three layers, decodes into a typed structure and validates it
// for the role. It returns every problem found, not the first, since a
// half-configured deployment usually is in several places.
func Load(role Role, opts Options) (*Config, error) {
	k := koanf.New(delim)

	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}

	if path := opts.path(); path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
				return nil, fmt.Errorf("load %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	if err := k.Load(env.Provider(delim, env.Opt{
		Prefix:        prefix,
		EnvironFunc:   opts.environ,
		TransformFunc: transform,
	}), nil); err != nil {
		return nil, fmt.Errorf("load environment: %w", err)
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result: &cfg,
			Squash: true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
			),
			// An unknown key is a typo, i.e. a setting someone believes they changed.
			ErrorUnused:      true,
			WeaklyTypedInput: true,
		},
	}); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}

	if err := cfg.Validate(role); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// transform turns RECON_DATABASE__MIGRATION_URL into database.migration_url.
// The provider drops an empty name, which keeps RECON_CONFIG out of the decoded
// structure.
func transform(key, value string) (string, any) {
	if key == configVar {
		return "", nil
	}
	trimmed, ok := strings.CutPrefix(key, prefix)
	if !ok || trimmed == "" {
		return "", nil
	}
	return strings.ReplaceAll(strings.ToLower(trimmed), nest, delim), value
}

// Validate checks the whole configuration for one role. It never mutates.
func (c *Config) Validate(role Role) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Env {
	case EnvDev, EnvStaging, EnvProd:
	default:
		fail("env must be one of %s, %s or %s, got %q", EnvDev, EnvStaging, EnvProd, c.Env)
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		fail("log.level must be debug, info, warn or error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		fail("log.format must be json or text, got %q", c.Log.Format)
	}

	switch role {
	case RoleControlPlane:
		if c.HTTP.Addr == "" {
			fail("http.addr is required")
		}
		if c.HTTP.ReadTimeout <= 0 {
			fail("http.read_timeout must be positive, got %s", c.HTTP.ReadTimeout)
		}
		if c.HTTP.ShutdownTimeout <= 0 {
			fail("http.shutdown_timeout must be positive, got %s", c.HTTP.ShutdownTimeout)
		}
		if c.Database.URL == "" {
			fail("database.url is required: the control plane connects as the application role")
		}
		// Required because its absence has no safe reading: the loops serving every
		// tenant would run as the application role, which row-level security answers
		// with zero rows. No partition created, no run expired, nothing notified, all
		// looking healthy.
		if c.Database.SystemURL == "" {
			fail("database.system_url is required: the loops that serve every tenant in one tick " +
				"connect as the system role, and under row-level security the application role " +
				"answers them with zero rows rather than with an error")
		}
		if c.Database.SystemMaxConns <= 0 {
			fail("database.system_max_conns must be positive, got %d", c.Database.SystemMaxConns)
		}
		if c.Database.SystemURL == c.Database.URL {
			fail("database.system_url and database.url are the same credential, so the role that " +
				"crosses tenants is the role that must not: the separation is a fact of deployment " +
				"rather than a naming convention")
		}
		// A security property, not a check.
		if c.Database.MigrationURL != "" {
			fail("database.migration_url must not be set on the control plane: " +
				"separating the roles buys nothing if the owner credentials sit " +
				"in the same environment as the application ones")
		}
		if c.Database.MaxConns <= 0 {
			fail("database.max_conns must be positive, got %d", c.Database.MaxConns)
		}
		if c.Database.ConnectTimeout <= 0 {
			fail("database.connect_timeout must be positive, got %s", c.Database.ConnectTimeout)
		}
		if len(c.Security.SigningKey) < MinSigningKeyLength {
			fail("security.signing_key must be at least %d bytes: it is what makes a run's "+
				"credentials unforgeable, and a guessable key is an unsigned one", MinSigningKeyLength)
		}
		if c.Maintenance.Interval <= 0 {
			fail("maintenance.interval must be positive, got %s", c.Maintenance.Interval)
		}
		// Validated whether or not a feed is configured, so turning the matcher on later
		// does not refuse its tunables at startup.
		if c.Verification.CandidateTimeout > c.Verification.Timeout {
			fail("verification.candidate_timeout (%s) exceeds verification.timeout (%s): the "+
				"candidate lane exists so a candidate does not wait behind a sweep, and a slot "+
				"held longer than one is that wait wearing a different name",
				c.Verification.CandidateTimeout, c.Verification.Timeout)
		}
		if c.CT.Interval <= 0 || c.CT.Window <= 0 || c.CT.CacheTTL <= 0 {
			fail("ct.interval, ct.window and ct.cache_ttl must all be positive")
		}
		if c.CT.Ceiling <= 0 || c.CT.CacheSize <= 0 {
			fail("ct.ceiling and ct.cache_size must be positive: a feed with no ceiling decides " +
				"how large a customer's inventory is")
		}
		for name, value := range map[string]time.Duration{
			"resolve": c.Verification.Resolve, "full": c.Verification.Full,
			"fingerprint": c.Verification.Fingerprint, "inactive": c.Verification.Inactive,
			"timeout": c.Verification.Timeout, "sweep_interval": c.Verification.SweepInterval,
			"candidate_timeout": c.Verification.CandidateTimeout,
			"discovery_retry":   c.Verification.DiscoveryRetry,
			"report_timeout":    c.Verification.ReportTimeout,
			"full_floor":        c.Verification.FullFloor,
			"render_sole":       c.Verification.RenderSole,
			"render_recovery":   c.Verification.RenderRecovery,
			"render_blind":      c.Verification.RenderBlind,
		} {
			if value <= 0 {
				fail("verification.%s must be positive, got %s", name, value)
			}
		}
		if c.Verification.Jitter < 0 {
			fail("verification.jitter must not be negative, got %s", c.Verification.Jitter)
		}
		if c.Verification.BatchSize <= 0 {
			fail("verification.batch_size must be positive, got %d", c.Verification.BatchSize)
		}
		if c.Verification.Ports == "" {
			fail("verification.ports is required: the port list travels in the run " +
				"definition so that discovery and verification scan the same ports")
		}
		for name, value := range map[string]time.Duration{
			"timeout": c.Render.Timeout, "interval": c.Render.Interval,
			"replan_spread": c.Render.ReplanSpread,
		} {
			if value <= 0 {
				fail("render.%s must be positive, got %s", name, value)
			}
		}
		// Seconds, the unit the statement spreads over: below one second it truncates
		// to zero, the modulo divides by it, and every replan returns a 500.
		if c.Render.ReplanSpread < time.Second {
			fail("render.replan_spread must be at least a second, got %s", c.Render.ReplanSpread)
		}
		if c.Render.Batch <= 0 || c.Render.Concurrency <= 0 || c.Render.Cost <= 0 {
			fail("render.batch, render.concurrency and render.cost must all be positive")
		}
		if c.Render.UnobservableAlert <= 0 || c.Render.UnobservableAlert > 1 {
			fail("render.unobservable_alert must be a share in (0, 1], got %v", c.Render.UnobservableAlert)
		}
		switch c.Runner.Provider {
		case RunnerNone:
		case RunnerScaleway:
			// Each would start a run against nothing, surfacing as a scanner that never
			// appears and not as a missing setting.
			if c.Runner.Region == "" {
				fail("runner.region is required with the %s runner", RunnerScaleway)
			}
			if c.Runner.JobID == "" {
				fail("runner.job_id is required with the %s runner: the definition is deployed "+
					"out of band and this names it", RunnerScaleway)
			}
			if c.Runner.SecretKey == "" {
				fail("runner.secret_key is required with the %s runner", RunnerScaleway)
			}
			if c.Runner.Endpoint == "" {
				fail("runner.endpoint is required with the %s runner", RunnerScaleway)
			}
			if c.Runner.Timeout <= 0 {
				fail("runner.timeout must be positive, got %s", c.Runner.Timeout)
			}
		default:
			fail("runner.provider must be %s or %s, got %q", RunnerNone, RunnerScaleway, c.Runner.Provider)
		}

		switch c.Notify.MinPriority {
		case "critical", "high", "medium", "low":
		default:
			fail("notify.min_priority must be critical, high, medium or low, got %q", c.Notify.MinPriority)
		}
		if c.Notify.Interval <= 0 || c.Notify.Timeout <= 0 || c.Notify.Batch <= 0 {
			fail("notify.interval, notify.timeout and notify.batch must all be positive")
		}
		// The floor is the first re-emission tier; below it the mass tip would never
		// speak, and the setting would pass its check and be silently overridden.
		if floor := notify.UnobservableTiers[0]; c.Notify.UnobservableAlert < floor || c.Notify.UnobservableAlert > 1 {
			fail("notify.unobservable_alert must be a share in [%v, 1], because %v is the first "+
				"tier a mass tip re-emits at and nothing below it can ever fire, got %v",
				floor, floor, c.Notify.UnobservableAlert)
		}

		// A run reaches this control plane from elsewhere, so this cannot be derived
		// from the listen address. Without it the scanner cannot fetch its targets, and
		// that would not look like a missing setting.
		if c.Verification.PublicURL == "" {
			fail("verification.public_url is required: it is where a run fetches its " +
				"target list and posts its report")
		}

	case RoleMigrate:
		if c.Database.MigrationURL == "" {
			fail("database.migration_url is required: the migrator runs as the owner")
		}
		// Symmetric to the refusal above: this process must not hold the application
		// credential either.
		if c.Database.URL != "" {
			fail("database.url must not be set on the migrator: it runs as the owner")
		}
		if c.Database.SystemURL != "" {
			fail("database.system_url must not be set on the migrator: it runs as the owner")
		}

	default:
		fail("unknown role %q", role)
	}

	return errors.Join(errs...)
}

// IsDev reports whether options refused outside development may be used. A
// method so adding an environment does not mean auditing every comparison.
func (c *Config) IsDev() bool { return c.Env == EnvDev }
