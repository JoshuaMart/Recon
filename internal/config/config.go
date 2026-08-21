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

	Maintenance Maintenance `koanf:"maintenance"`
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
