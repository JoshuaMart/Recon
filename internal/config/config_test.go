package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/JoshuaMart/recon/internal/config"
)

// environ builds an environment for a test without touching the process's own,
// which is what lets every test here run in parallel.
func environ(vars ...string) func() []string {
	return func() []string { return vars }
}

func TestAnEmptyEnvironmentStillProducesAWorkingConfiguration(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(config.RoleMigrate, config.Options{
		Environ: environ("RECON_DATABASE__MIGRATION_URL=postgres://owner@localhost/recon"),
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Env != config.EnvDev {
		t.Errorf("env = %q, want %q", cfg.Env, config.EnvDev)
	}
	if cfg.HTTP.ReadTimeout != 10*time.Second {
		t.Errorf("http.read_timeout = %s, want 10s", cfg.HTTP.ReadTimeout)
	}
}

func TestTwoUnderscoresNestAndOneDoesNot(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(config.RoleControlPlane, config.Options{
		Environ: environ(
			"RECON_DATABASE__URL=postgres://app@localhost/recon",
			"RECON_SECURITY__SIGNING_KEY=a-signing-key-long-enough-to-be-one",
			"RECON_VERIFICATION__PUBLIC_URL=https://recon.example",
			// One underscore inside a key, two between levels. If the
			// transform confused the two, this would land nowhere and the
			// default would survive.
			"RECON_HTTP__READ_TIMEOUT=42s",
			"RECON_DATABASE__MAX_CONNS=25",
		),
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.HTTP.ReadTimeout != 42*time.Second {
		t.Errorf("http.read_timeout = %s, want 42s", cfg.HTTP.ReadTimeout)
	}
	if cfg.Database.MaxConns != 25 {
		t.Errorf("database.max_conns = %d, want 25", cfg.Database.MaxConns)
	}
}

// The refusal that is a security property rather than a check: role separation
// buys nothing if whoever reaches execution in the control plane finds the
// owner credentials next to the application ones.
func TestTheControlPlaneRefusesTheOwnerCredential(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.RoleControlPlane, config.Options{
		Environ: environ(
			"RECON_DATABASE__URL=postgres://app@localhost/recon",
			"RECON_DATABASE__MIGRATION_URL=postgres://owner@localhost/recon",
			"RECON_SECURITY__SIGNING_KEY=a-signing-key-long-enough-to-be-one",
			"RECON_VERIFICATION__PUBLIC_URL=https://recon.example",
		),
	})
	if err == nil {
		t.Fatal("expected the control plane to refuse database.migration_url")
	}
	if !strings.Contains(err.Error(), "migration_url") {
		t.Errorf("the error does not name the offending option: %v", err)
	}
}

func TestTheMigratorRefusesTheApplicationCredential(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.RoleMigrate, config.Options{
		Environ: environ(
			"RECON_DATABASE__MIGRATION_URL=postgres://owner@localhost/recon",
			"RECON_DATABASE__URL=postgres://app@localhost/recon",
		),
	})
	if err == nil {
		t.Fatal("expected the migrator to refuse database.url")
	}
}

// A typo in configuration is a setting somebody believes they changed.
func TestAnUnknownOptionIsRefused(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.RoleMigrate, config.Options{
		Environ: environ(
			"RECON_DATABASE__MIGRATION_URL=postgres://owner@localhost/recon",
			"RECON_DATABSE__URL=typo",
		),
	})
	if err == nil {
		t.Fatal("expected an unknown option to be refused")
	}
}

// A half-configured deployment is usually half-configured in several places.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.RoleControlPlane, config.Options{
		Environ: environ(
			"RECON_ENV=production",
			"RECON_LOG__LEVEL=chatty",
		),
	})
	if err == nil {
		t.Fatal("expected the load to fail")
	}

	for _, want := range []string{"env", "log.level", "database.url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s:\n%v", want, err)
		}
	}
}

// "production" is not "prod", and an unknown environment must fail rather than
// fall through to the permissive side: several options are refused outside dev.
func TestAnUnknownEnvironmentDoesNotDefaultToPermissive(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults()
	cfg.Env = "production"
	if cfg.IsDev() {
		t.Error("an unknown environment reads as dev")
	}
	if err := cfg.Validate(config.RoleMigrate); err == nil {
		t.Error("expected an unknown environment to be refused")
	}
}

// RECON_CONFIG says where to read, not what to be, so it must not reach the
// decoded structure. With ErrorUnused on, a leak shows up as a failed load.
func TestTheConfigPathIsNotAnOption(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.RoleMigrate, config.Options{
		Environ: environ(
			"RECON_CONFIG=/nonexistent/recon.yaml",
			"RECON_DATABASE__MIGRATION_URL=postgres://owner@localhost/recon",
		),
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
}

// A guessable key is an unsigned one, so the refusal happens at startup rather
// than on the first run the control plane tries to authenticate.
func TestTheControlPlaneRefusesAShortSigningKey(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.RoleControlPlane, config.Options{
		Environ: environ(
			"RECON_DATABASE__URL=postgres://app@localhost/recon",
			"RECON_SECURITY__SIGNING_KEY=short",
		),
	})
	if err == nil {
		t.Fatal("a five byte signing key was accepted")
	}
	if !strings.Contains(err.Error(), "signing_key") {
		t.Errorf("the error does not name the option: %v", err)
	}
}
