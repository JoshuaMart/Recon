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
	PublicURL string   `koanf:"public_url"`
	APIKey    string   `koanf:"api_key"`
	HTTP      HTTP     `koanf:"http"`
	Database  Database `koanf:"database"`
	Log       Log      `koanf:"log"`
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
	return errors.Join(errs...)
}
