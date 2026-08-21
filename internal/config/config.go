// Package config is the only place in this repository that reads the
// environment.
//
// That is enforced by the linter rather than by convention, because without an
// automatic constraint the rule holds for about three weeks. Everything else
// receives a *Config; nothing reaches for a package-level global.
//
// Merge order is defaults in code, then a YAML file, then environment
// variables prefixed RECON_. The env name of an option is its path uppercased
// with "." replaced by "__", so database.migration_url is
// RECON_DATABASE__MIGRATION_URL. Two underscores nest, one stays part of a key.
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
)

// Role is which binary is asking. It decides what a configuration must carry
// and, more importantly, what it must not: the separation of database
// credentials is a fact of deployment rather than a naming convention, so it
// is checked here instead of being left to whoever writes the next compose
// file.
type Role string

const (
	// RoleControlPlane serves the API and runs the background loops.
	RoleControlPlane Role = "controlplane"
	// RoleMigrate applies and rolls back migrations, as the owner.
	RoleMigrate Role = "migrate"
)

// MinSigningKeyLength is what the signing key has to reach. It mirrors what the
// signer itself enforces, so a deployment finds out at startup rather than on
// the first run it tries to authenticate.
const MinSigningKeyLength = 32

// Runners a deployment can start runs with.
const (
	// RunnerNone renders the definition and starts nothing.
	RunnerNone = "none"
	// RunnerScaleway starts a serverless job definition.
	RunnerScaleway = "scaleway"
)

// Environments the configuration accepts. Several options are refused outside
// dev, so an unknown value must fail rather than default to the permissive
// side.
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
	Verification Verification `koanf:"verification"`
	Render       Render       `koanf:"render"`
	Runner       Runner       `koanf:"runner"`
	Notify       Notify       `koanf:"notify"`
}

// Notify is where alerts go and how often they leave.
//
// One generic webhook and only that. Discord and Slack are a payload template
// rather than a connector: writing one in Go would freeze what a template
// expresses and start over at the next one.
type Notify struct {
	// WebhookURL is the configured channel. Empty means an organization with no
	// channel, which is a deliberate configuration rather than an outage:
	// events are marked delivered and counted, so "computed and sent nowhere"
	// stays visible.
	WebhookURL string `koanf:"webhook_url"`
	// Template renders the body. Empty takes the default, which is the shape
	// Discord and Slack both accept.
	Template string `koanf:"template"`
	// MinPriority is the floor the configured channel wants.
	MinPriority string `koanf:"min_priority"`
	// Interval is how often the queue is drained. A takeover is sent with at
	// most one tick between being written and being sent, so this is what
	// "immediate" actually means.
	Interval time.Duration `koanf:"interval"`
	Batch    int           `koanf:"batch"`
	Timeout  time.Duration `koanf:"timeout"`
	// UnobservableAlert is the share of a programme's inventory that makes a
	// mass tip an event.
	UnobservableAlert float64 `koanf:"unobservable_alert"`
}

// Runner is how a run definition is actually started.
//
// The control plane starts, it never updates. The call that modifies a
// definition replaces its whole environment map rather than merging into it, so
// a control plane that wrote its overrides that way would wipe the source API
// keys the definition carries, and nothing would fail: the next run would
// simply query fewer sources and find less. Only the start call is made here,
// and it takes its overrides per run.
type Runner struct {
	// Provider is "none" or "scaleway". None renders the definition and starts
	// nothing, which is the development shape: a person runs the image with
	// what the console shows. It is the same shape as production minus the
	// call, which is what keeps the local path from becoming a second way of
	// starting a run.
	Provider string `koanf:"provider"`
	Region   string `koanf:"region"`
	// JobID is the definition to start. It is deployed once, out of band, and
	// this names it.
	JobID string `koanf:"job_id"`
	// SecretKey is the only credential, and it is scoped to starting this
	// definition and to nothing else on the account. It sits in the process
	// that already holds the inventory, so how narrow it is is the only thing
	// bounding the damage of that process being compromised.
	SecretKey string `koanf:"secret_key"`
	// Endpoint is the API base, so a test does not call a cloud.
	Endpoint string        `koanf:"endpoint"`
	Timeout  time.Duration `koanf:"timeout"`
}

// Render is the browser side of verification.
//
// It is a separate block from verification because it answers to a different
// unit: a probe is milliseconds and a render is seconds and several hundred
// megabytes, so the two cannot share a batch size or a cadence without one of
// them being wrong.
type Render struct {
	// URL is where the rendering service listens. Empty turns the loop off,
	// which is the one honest way to run without a browser: the assets keep
	// their due dates and nothing pretends to have looked.
	URL     string        `koanf:"url"`
	Timeout time.Duration `koanf:"timeout"`
	// Interval is how often the pass runs.
	Interval time.Duration `koanf:"interval"`
	Batch    int           `koanf:"batch"`
	// Concurrency is set above where the budget binds rather than at it, so
	// the thing throttling a programme is its published rate limit and not a
	// number nobody calibrated.
	Concurrency int `koanf:"concurrency"`
	// Cost is what one render is charged against a programme's rate limit. A
	// browser fetches a page and then everything the page pulls, so billing it
	// as one request would make the most expensive thing in the system the
	// cheapest on the counter.
	Cost int `koanf:"cost"`
	// UnobservableAlert is the share of a programme's inventory that turns a
	// mass tip into an alert. A mass tip says something about the observer
	// rather than about the targets.
	UnobservableAlert float64 `koanf:"unobservable_alert"`
	// ReplanSpread is how long a forced refresh after a service update is
	// spread over. It exists to restore baseline consistency without a mass
	// alert, and doing that in an hour would be the mass alert.
	ReplanSpread time.Duration `koanf:"replan_spread"`
}

// Verification is the loop that keeps the inventory honest, and the shape of
// the runs it provisions.
//
// The cadences are configuration rather than constants because the stage ladder
// is the cost knob: a resolve is one round trip to the resolver pool and a full
// is a hundred connections per host, so the right numbers depend on the size of
// a perimeter and on what the programme allows.
type Verification struct {
	Resolve     time.Duration `koanf:"resolve"`
	Full        time.Duration `koanf:"full"`
	Fingerprint time.Duration `koanf:"fingerprint"`
	Inactive    time.Duration `koanf:"inactive"`
	Jitter      time.Duration `koanf:"jitter"`
	// RenderSole, RenderRecovery and RenderBlind are the cadences of the three
	// regimes that are not the nominal one. They are separate values because
	// each answers a different question: who is still detecting, and is a
	// render a measurement or a recovery attempt.
	RenderSole     time.Duration `koanf:"render_sole"`
	RenderRecovery time.Duration `koanf:"render_recovery"`
	RenderBlind    time.Duration `koanf:"render_blind"`
	// FullFloor is how often a failing asset's port sweep may run at its
	// fastest. A backoff curve is written for the cheap rung, and applying it
	// unchanged to the expensive one turns a confirmation into a flood.
	FullFloor time.Duration `koanf:"full_floor"`
	// BatchSize is how many hosts one run freezes. It is what actually bounds
	// a pass: a due date decides eligibility, and this decides what goes out.
	BatchSize int `koanf:"batch_size"`
	// Timeout is a run's own budget, and it has to expire before the
	// platform's. The outer bound sits on a job definition Recon does not
	// control, so this value has to be set to match: a run killed from outside
	// delivers nothing, where one that runs out of its own time still delivers
	// a truncated report.
	Timeout time.Duration `koanf:"timeout"`
	// Grace is how long past the deadline a report is still worth ingesting.
	// The data is valid either way; the run may simply have been
	// re-dispatched.
	Grace time.Duration `koanf:"grace"`
	// SweepInterval is how often runs past their deadline are expired. A run
	// nothing expires holds its targets forever.
	SweepInterval time.Duration `koanf:"sweep_interval"`
	// DiscoveryRetry is how long after a discovery run that delivered nothing
	// a replacement may be provisioned. It is much shorter than the discovery
	// interval, because a run that failed in thirty seconds must not cost a
	// week of coverage, and much longer than the tick, because a permanently
	// broken runner would otherwise provision and bill on every one.
	DiscoveryRetry time.Duration `koanf:"discovery_retry"`
	// Ports is the curated list, and it is data rather than code. It travels
	// in the run definition so discovery and verification scan the same ports:
	// a second list configured on the scanner would be a second list to keep
	// in agreement, and nothing would raise an error when the two diverged.
	Ports string `koanf:"ports"`
	// PublicURL is the base a run reaches this control plane on, for its
	// target list and for its report. It is not derivable from the listen
	// address: the scanner runs somewhere else entirely.
	PublicURL string `koanf:"public_url"`
}

// Security holds the one secret the control plane signs with.
//
// A run's credentials are HMACs over the run, the purpose and an expiry, so
// there is nothing to store, nothing to revoke and nothing to purge. The whole
// of that rests on this key, which is why a short one is refused: an HMAC whose
// key can be guessed lets anyone mint a token for any run.
type Security struct {
	SigningKey string `koanf:"signing_key"`
}

// Maintenance is the housekeeping tick.
//
// It has no enable flag on purpose. A partition job that can be turned off is
// an ingestion outage three months later, triggered by a button that talks
// about something else.
type Maintenance struct {
	Interval time.Duration `koanf:"interval"`
}

// Log controls what the process writes to stderr. Everything is structured and
// carries a correlation id from ingestion onwards; grafting that on later means
// touching every write path.
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

// Database holds one connection string per role. They are separate variables
// rather than one string with a switch, because the role a process connects
// with is chosen when a pool is opened and not case by case in the code.
type Database struct {
	// URL is asm_app: the control plane's requests, subject to row-level
	// security once it exists.
	URL string `koanf:"url"`
	// SystemURL is asm_sys: the background loops that serve every tenant in
	// one tick. Empty until the phase that introduces them.
	SystemURL string `koanf:"system_url"`
	// MigrationURL is asm_owner. It must never be in the control plane's
	// environment: role separation buys nothing if whoever reaches execution
	// there finds the owner credentials next to the application ones.
	MigrationURL string `koanf:"migration_url"`

	MaxConns       int32         `koanf:"max_conns"`
	ConnectTimeout time.Duration `koanf:"connect_timeout"`
}

// Enrich points at the Geo-IP databases.
//
// Both paths are optional and a deployment with neither is a normal
// deployment. What matters is that the difference is visible: the console
// cannot tell "not configured" from "configured with no match" by looking at
// the data, since both leave an asset without an operator.
type Enrich struct {
	CityDatabase string `koanf:"city_database"`
	ASNDatabase  string `koanf:"asn_database"`
}

// Defaults are the values a deployment inherits when it says nothing. They
// live in code rather than in a shipped file so that an empty environment is
// still a working one.
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
			ConnectTimeout: 5 * time.Second,
		},
		Maintenance: Maintenance{Interval: time.Hour},
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
			Resolve:        24 * time.Hour,
			Full:           72 * time.Hour,
			Fingerprint:    21 * 24 * time.Hour,
			RenderSole:     7 * 24 * time.Hour,
			RenderRecovery: 30 * 24 * time.Hour,
			RenderBlind:    7 * 24 * time.Hour,
			Inactive:       7 * 24 * time.Hour,
			Jitter:         15 * time.Minute,
			FullFloor:      6 * time.Hour,
			BatchSize:      500,
			Timeout:        30 * time.Minute,
			Grace:          10 * time.Minute,
			SweepInterval:  time.Minute,
			DiscoveryRetry: time.Hour,
			// Not nmap's top 100, which orders ports by how often they are
			// open across the whole internet and therefore leads with mail and
			// printing. The criterion here is that a port earns its place if
			// it can carry a finding.
			Ports: "80,443,8080,8443,8000,8090,9000,9443,3000,5000," +
				"2375,2376,6443,10250,15672,5601,8983,9990,2083,2087," +
				"3306,5432,27017,6379,9200,11211,9042,2379," +
				"3389,5900,1080,3128",
		},
	}
}

// Options adjust where a configuration is read from. The zero value reads the
// process environment and the file RECON_CONFIG names, which is what every
// binary uses: no command reaches for the environment itself.
type Options struct {
	// File is an optional YAML path, overriding RECON_CONFIG. Missing is not
	// an error: a deployment configured entirely through the environment is
	// the normal case.
	File string
	// Environ overrides the source of environment variables. Tests set it so
	// that they never touch the process environment, which is also why this
	// package can be tested in parallel.
	Environ func() []string
}

// configVar names the file to read. It is deliberately not an option of the
// structure: a configuration cannot say where it is loaded from.
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

// Load merges the three layers, decodes into a typed structure and validates
// it for the role that asked. It returns every problem it found rather than
// the first, because a half-configured deployment is usually half-configured
// in several places at once.
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
			// An unknown key is a typo, and a typo in configuration is a
			// setting somebody believes they changed. Failing on it costs one
			// error message; accepting it costs an afternoon.
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
// An empty name is dropped by the provider, which is how RECON_CONFIG stays
// out of the decoded structure: it says where to read, not what to be.
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
		// The one refusal that is a security property rather than a check.
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
		for name, value := range map[string]time.Duration{
			"resolve": c.Verification.Resolve, "full": c.Verification.Full,
			"fingerprint": c.Verification.Fingerprint, "inactive": c.Verification.Inactive,
			"timeout": c.Verification.Timeout, "sweep_interval": c.Verification.SweepInterval,
			"discovery_retry": c.Verification.DiscoveryRetry,
			"full_floor":      c.Verification.FullFloor,
			"render_sole":     c.Verification.RenderSole,
			"render_recovery": c.Verification.RenderRecovery,
			"render_blind":    c.Verification.RenderBlind,
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
		// Seconds, because that is the unit the statement spreads over. Under
		// one second it truncates to zero and the modulo divides by it, which
		// turns every replan into a 500.
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
			// Each of these is a way for a run to be started against nothing,
			// and the failure would arrive as a scanner that never appears
			// rather than as a missing setting.
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

		// A run reaches this control plane from somewhere else entirely, so
		// this cannot be derived from the listen address. Without it a run
		// definition names nowhere, and the failure would surface as a scanner
		// that cannot fetch its targets rather than as a missing setting.
		switch c.Notify.MinPriority {
		case "critical", "high", "medium", "low":
		default:
			fail("notify.min_priority must be critical, high, medium or low, got %q", c.Notify.MinPriority)
		}
		if c.Notify.Interval <= 0 || c.Notify.Timeout <= 0 || c.Notify.Batch <= 0 {
			fail("notify.interval, notify.timeout and notify.batch must all be positive")
		}
		if c.Notify.UnobservableAlert <= 0 || c.Notify.UnobservableAlert > 1 {
			fail("notify.unobservable_alert must be a share in (0, 1], got %v", c.Notify.UnobservableAlert)
		}

		if c.Verification.PublicURL == "" {
			fail("verification.public_url is required: it is where a run fetches its " +
				"target list and posts its report")
		}

	case RoleMigrate:
		if c.Database.MigrationURL == "" {
			fail("database.migration_url is required: the migrator runs as the owner")
		}
		// Symmetric to the refusal above. This process has no business holding
		// the application credential either.
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

// IsDev reports whether options refused outside development may be used. It is
// a method rather than a comparison at each call site so that adding an
// environment does not mean auditing every one of them.
func (c *Config) IsDev() bool { return c.Env == EnvDev }
