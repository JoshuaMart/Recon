// Package compose holds no code. It exists so that the network split is
// guarded by a test rather than by whoever reads the compose file next.
//
// The script beside it proves the isolation holds on a running stack. This
// proves nobody undid it in the file since, which is a different question and
// the one a pull request can answer.
package compose

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// isolated are the services that render pages controlled by a target.
var isolated = []string{"fingerprinter", "chrome-1"}

// unreachable names what must stay out of their reach.
var unreachable = []string{"postgres", "controlplane"}

type composeFile struct {
	Services map[string]struct {
		Networks    []string          `yaml:"networks"`
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
	Networks map[string]any `yaml:"networks"`
}

func load(t *testing.T) composeFile {
	t.Helper()

	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}

	var parsed composeFile
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse compose file: %v", err)
	}
	return parsed
}

func networksOf(t *testing.T, file composeFile, service string) []string {
	t.Helper()

	svc, ok := file.Services[service]
	if !ok {
		t.Fatalf("service %q is not in the compose file", service)
	}
	// An empty list puts the service on the default network with everything
	// else, which is the failure this whole test exists to catch.
	if len(svc.Networks) == 0 {
		t.Fatalf("service %q declares no network, so it joins the default one "+
			"together with the database", service)
	}
	return svc.Networks
}

func TestTheRenderingServiceSharesNoNetworkWithTheInventory(t *testing.T) {
	file := load(t)

	for _, service := range isolated {
		for _, network := range networksOf(t, file, service) {
			for _, target := range unreachable {
				for _, other := range networksOf(t, file, target) {
					if network == other {
						t.Errorf("%s and %s share network %q, and the architecture requires no route",
							service, target, network)
					}
				}
			}
		}
	}
}

// The positive control. Without it the test above passes just as happily on a
// compose file where the services were renamed or removed, which is how an
// isolation test usually goes quietly green.
func TestTheControlPlaneStillReachesTheDatabase(t *testing.T) {
	file := load(t)

	for _, network := range networksOf(t, file, "controlplane") {
		for _, other := range networksOf(t, file, "postgres") {
			if network == other {
				return
			}
		}
	}
	t.Error("the control plane shares no network with postgres, so the isolation " +
		"test above proves nothing")
}

// A service holding a credential is a service whose network confinement is one
// bug away from irrelevant. Checked on the environment rather than on the code,
// because that is where a credential would actually be handed over, and because
// it is the edit somebody makes in a hurry.
func TestTheRenderingServiceHoldsNoCredential(t *testing.T) {
	file := load(t)

	// Substrings rather than exact names: the point is to catch a variable
	// nobody thought to list here, whatever a future integration calls it.
	forbidden := []string{
		"PASSWORD", "SECRET", "TOKEN", "ACCESS_KEY", "SIGNING_KEY",
		"DATABASE", "POSTGRES", "STORAGE", "S3",
	}

	for _, name := range isolated {
		service := file.Services[name]
		for key, value := range service.Environment {
			upper := strings.ToUpper(key)
			for _, pattern := range forbidden {
				if strings.Contains(upper, pattern) {
					t.Errorf("%s is given %s, and it is meant to hold nothing: "+
						"everything it needs arrives in the scan request", name, key)
				}
			}
			for _, host := range unreachable {
				if strings.Contains(strings.ToLower(value), host) {
					t.Errorf("%s is given %s=%s, which names %s", name, key, value, host)
				}
			}
		}
	}
}

// The migration credential must not be in the control plane's environment.
// The configuration refuses to start with it, and this catches the edit before
// anybody starts anything.
func TestTheControlPlaneIsNotGivenTheOwnerCredential(t *testing.T) {
	file := load(t)

	for key := range file.Services["controlplane"].Environment {
		if strings.Contains(strings.ToUpper(key), "MIGRATION_URL") {
			t.Errorf("the control plane is given %s: separating the roles buys "+
				"nothing if both credentials sit in the same environment", key)
		}
	}
}

// FastRecon is a one-shot job started with per-run overrides, not a daemon.
// Listing it here would shape the local stack around a service that has
// nothing to listen to.
func TestTheScannerIsNotAService(t *testing.T) {
	file := load(t)

	for name := range file.Services {
		if strings.Contains(strings.ToLower(name), "fastrecon") {
			t.Errorf("%q is a compose service: a run is an image started with "+
				"overrides, and a daemon here would be a second way to start one", name)
		}
	}
}

// The rendering service holds no credential, and that is checked rather than
// intended.
//
// It executes JavaScript controlled by the target, so anything it holds is
// something a page it renders can try to reach. Everything it needs arrives in
// the scan request, which is what makes "holds nothing" a shape rather than a
// promise: there is no variable to leak because there is no variable.
func TestTheRenderingSideHoldsNoCredential(t *testing.T) {
	parsed := load(t)

	// Names that would carry one. Matched on the name rather than the value,
	// because the value is an interpolation at this point and reading it here
	// would test the shell rather than the file.
	secretish := []string{"password", "secret", "token", "key", "url", "dsn", "credential"}

	for _, name := range isolated {
		service, ok := parsed.Services[name]
		if !ok {
			t.Fatalf("%s is not in the compose file", name)
		}
		for variable := range service.Environment {
			lower := strings.ToLower(variable)
			for _, needle := range secretish {
				if strings.Contains(lower, needle) {
					t.Errorf("%s carries %s, and everything it needs arrives in the request", name, variable)
				}
			}
		}
	}

	// And the file it is given is configuration rather than a secret store.
	config, err := os.ReadFile("fingerprinter.yml")
	if err != nil {
		t.Fatalf("read the rendering config: %v", err)
	}
	for _, needle := range []string{"password:", "secret:", "token:", "api_key:"} {
		if strings.Contains(strings.ToLower(string(config)), needle) {
			t.Errorf("the rendering config carries %q", needle)
		}
	}
}
