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

// browsers are the pool the rendering service drives. Listed rather than
// inferred, because a browser added to a compose file and not here is a browser
// no test says anything about, which is the quiet way an isolated set stops
// covering what it names.
var browsers = []string{"chrome-1", "chrome-2", "chrome-3"}

// isolated are the services whose input is controlled by somebody else and
// which therefore hold nothing: the browsers and the service driving them, and
// the Certificate Transparency feed, which parses X.509 anybody can get logged.
var isolated = append([]string{"fingerprinter", "certstream"}, browsers...)

// The two files, and what the rendering side must stay away from in each.
//
// They differ in one entry, and the difference is a decision rather than an
// oversight. One host has no way to express a link that works in one direction
// only, so the production file gives the control plane and the fingerprinter a
// network of their own and the fingerprinter can resolve `controlplane` on it.
// The local file needs no such thing, because Docker Desktop proxies a
// published port to the host's loopback, and it keeps the stricter shape.
//
// The database is in both lists, and the browser, which is where a target's
// JavaScript actually runs, is covered by the test below on top of this one:
// it reaches nothing in either file.
var topologies = []struct {
	file        string
	unreachable []string
}{
	{"compose.yaml", []string{"postgres", "controlplane", "console"}},
	{"compose.prod.yaml", []string{"postgres", "console"}},
}

type composeFile struct {
	Services map[string]struct {
		Networks    []string          `yaml:"networks"`
		Environment map[string]string `yaml:"environment"`
		Ports       []string          `yaml:"ports"`
	} `yaml:"services"`
	Networks map[string]any `yaml:"networks"`
}

func load(t *testing.T, file string) composeFile {
	t.Helper()

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	var parsed composeFile
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return parsed
}

func networksOf(t *testing.T, parsed composeFile, service string) []string {
	t.Helper()

	svc, ok := parsed.Services[service]
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

// shares reports the networks two services have in common.
func shares(t *testing.T, parsed composeFile, a, b string) []string {
	t.Helper()

	var common []string
	for _, network := range networksOf(t, parsed, a) {
		for _, other := range networksOf(t, parsed, b) {
			if network == other {
				common = append(common, network)
			}
		}
	}
	return common
}

func TestTheRenderingServiceSharesNoNetworkWithTheInventory(t *testing.T) {
	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			for _, service := range isolated {
				for _, target := range topology.unreachable {
					for _, network := range shares(t, parsed, service, target) {
						t.Errorf("%s and %s share network %q, and the architecture requires no route",
							service, target, network)
					}
				}
			}
		})
	}
}

// The browser is held to the stricter rule in both files, and it is the one
// that matters most: the fingerprinter fetches what it is told to fetch, but
// Chrome executes whatever the page it renders contains. Everything the
// production file relaxes for the service driving it stays closed here.
func TestTheBrowserReachesNothingThatHoldsAnything(t *testing.T) {
	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			for _, browser := range browsers {
				for _, target := range []string{"postgres", "controlplane", "console"} {
					for _, network := range shares(t, parsed, browser, target) {
						t.Errorf("%s and %s share network %q: a browser runs code the target "+
							"wrote, so it reaches the service driving it and the internet, and "+
							"nothing else", browser, target, network)
					}
				}
			}
		})
	}
}

// The positive control. Without it the test above passes just as happily on a
// compose file where the services were renamed or removed, which is how an
// isolation test usually goes quietly green.
func TestTheControlPlaneStillReachesTheDatabase(t *testing.T) {
	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			if len(shares(t, parsed, "controlplane", "postgres")) == 0 {
				t.Error("the control plane shares no network with postgres, so the isolation " +
					"test above proves nothing")
			}
		})
	}
}

// The second positive control, and it belongs to the production file: there the
// control plane calls the rendering service by name, so a file where they share
// nothing is a file where rendering silently never happens.
func TestTheControlPlaneStillReachesTheRenderingService(t *testing.T) {
	parsed := load(t, "compose.prod.yaml")

	if len(shares(t, parsed, "controlplane", "fingerprinter")) == 0 {
		t.Error("the control plane shares no network with the fingerprinter, and it reaches " +
			"it by service name: every render would fail to resolve a host")
	}
}

// A service holding a credential is a service whose network confinement is one
// bug away from irrelevant. Checked on the environment rather than on the code,
// because that is where a credential would actually be handed over, and because
// it is the edit somebody makes in a hurry.
func TestTheRenderingServiceHoldsNoCredential(t *testing.T) {
	// Substrings rather than exact names: the point is to catch a variable
	// nobody thought to list here, whatever a future integration calls it.
	forbidden := []string{
		"PASSWORD", "SECRET", "TOKEN", "ACCESS_KEY", "SIGNING_KEY",
		"DATABASE", "POSTGRES", "STORAGE", "S3",
	}

	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			for _, name := range isolated {
				service := parsed.Services[name]
				for key, value := range service.Environment {
					upper := strings.ToUpper(key)
					for _, pattern := range forbidden {
						if strings.Contains(upper, pattern) {
							t.Errorf("%s is given %s, and it is meant to hold nothing: "+
								"everything it needs arrives in the scan request", name, key)
						}
					}
					// Both names, in both files. The production file lets the
					// fingerprinter resolve the control plane, and that is still
					// not a reason to hand it the address.
					for _, host := range []string{"postgres", "controlplane"} {
						if strings.Contains(strings.ToLower(value), host) {
							t.Errorf("%s is given %s=%s, which names %s", name, key, value, host)
						}
					}
				}
			}
		})
	}
}

// The migration credential must not be in the control plane's environment.
// The configuration refuses to start with it, and this catches the edit before
// anybody starts anything.
func TestTheControlPlaneIsNotGivenTheOwnerCredential(t *testing.T) {
	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			for key := range parsed.Services["controlplane"].Environment {
				if strings.Contains(strings.ToUpper(key), "MIGRATION_URL") {
					t.Errorf("the control plane is given %s: separating the roles buys "+
						"nothing if both credentials sit in the same environment", key)
				}
			}
		})
	}
}

// FastRecon is a one-shot job started with per-run overrides, not a daemon.
// Listing it here would shape the stack around a service that has nothing to
// listen to.
func TestTheScannerIsNotAService(t *testing.T) {
	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			for name := range parsed.Services {
				if strings.Contains(strings.ToLower(name), "fastrecon") {
					t.Errorf("%q is a compose service: a run is an image started with "+
						"overrides, and a daemon here would be a second way to start one", name)
				}
			}
		})
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
	// Names that would carry one. Matched on the name rather than the value,
	// because the value is an interpolation at this point and reading it here
	// would test the shell rather than the file.
	secretish := []string{"password", "secret", "token", "key", "url", "dsn", "credential"}

	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			for _, name := range isolated {
				service, ok := parsed.Services[name]
				if !ok {
					t.Fatalf("%s is not in %s", name, topology.file)
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
		})
	}

	// And the files they are given are configuration rather than secret stores.
	// Mounted by both stacks, so read once.
	for _, name := range []string{"fingerprinter.yml", "certstream.yml"} {
		config, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, needle := range []string{"password:", "secret:", "token:", "api_key:"} {
			if strings.Contains(strings.ToLower(string(config)), needle) {
				t.Errorf("%s carries %q", name, needle)
			}
		}
	}
}

// The console holds no database credential, and it is checked here rather than
// intended.
//
// That is the whole reason the interface is a process of its own rather than
// pages served by the control plane: server templates would put the rendering
// of the interface inside the process holding the database credentials, and a
// process that renders pages and can write to the inventory is one bug away
// from being the whole system.
//
// It reaches the control plane over HTTP with the organization's token, like
// any other API client, so the only variable it needs is an address.
func TestTheConsoleHoldsNoDatabaseCredential(t *testing.T) {
	forbidden := []string{"PASSWORD", "SECRET", "SIGNING_KEY", "DATABASE", "POSTGRES", "DSN"}

	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			service, ok := parsed.Services["console"]
			if !ok {
				t.Fatalf("the console is not in %s", topology.file)
			}

			for key, value := range service.Environment {
				upper := strings.ToUpper(key)
				for _, pattern := range forbidden {
					if strings.Contains(upper, pattern) {
						t.Errorf("the console is given %s, and it is meant to hold none: it reaches "+
							"the control plane over HTTP like any other API client", key)
					}
				}
				if strings.Contains(strings.ToLower(value), "postgres") {
					t.Errorf("the console is given %s=%s, which names the database", key, value)
				}
			}
		})
	}
}

// ORIGIN is set, and the reason it is checked in the file is that its absence
// does not fail at startup in every deployment shape: it fails at the first
// form, which is the worst moment and the hardest one to attribute.
func TestTheConsoleIsGivenAnOrigin(t *testing.T) {
	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			if _, set := parsed.Services["console"].Environment["ORIGIN"]; !set {
				t.Error("the console is given no ORIGIN: without it the node adapter rejects every " +
					"legitimate form POST, the login screen included")
			}
		})
	}
}

// The console is on the control network and never on the rendering one. It is
// the one component besides the control plane that holds a credential, and the
// browser side is the one component that must reach nothing.
func TestTheConsoleSharesNoNetworkWithTheRenderingSide(t *testing.T) {
	for _, topology := range topologies {
		t.Run(topology.file, func(t *testing.T) {
			parsed := load(t, topology.file)

			for _, name := range isolated {
				for _, network := range shares(t, parsed, "console", name) {
					t.Errorf("the console and %s share network %q", name, network)
				}
			}
		})
	}
}

// Nothing in the production file publishes a port. The reverse proxy reaches
// the two services that answer a hostname over a network it already sits on,
// and a published port there is a second way in that no route, no middleware
// and no certificate applies to.
//
// The local file publishes several on purpose, which is why this is asked of
// one file rather than of both.
func TestTheProductionFilePublishesNoPort(t *testing.T) {
	parsed := load(t, "compose.prod.yaml")

	for name, service := range parsed.Services {
		if len(service.Ports) > 0 {
			t.Errorf("%s publishes %v: on a public host that is a listener the reverse proxy "+
				"knows nothing about", name, service.Ports)
		}
	}
}
