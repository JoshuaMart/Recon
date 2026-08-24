package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The two lists this compares are the operations in openapi.yaml and the
// mux.Handle calls in main.go. 15.3 says they are two descriptions of one
// thing, maintained by hand, with nothing that fails to compile when they
// diverge. This is that nothing, written down.
//
// It cannot check the shape of a body or the meaning of a status, which is the
// part of the drift 15.3 says stays unbounded. It catches the one that is
// mechanical: a route added, removed or renamed without the document following.
const (
	routesFile  = "main.go"
	openAPIFile = "../../docs/public/openapi.yaml"
)

var routePattern = regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) ([^"]+)"`)

func TestTheDocumentAndTheMuxAreTwoListsOfOneThing(t *testing.T) {
	source, err := os.ReadFile(routesFile)
	if err != nil {
		t.Fatalf("read the routes: %v", err)
	}
	matches := routePattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no route was found in main.go, so this test would pass on an empty document")
	}

	served := map[string]bool{}
	for _, match := range matches {
		served[strings.ToLower(match[1])+" "+match[2]] = true
	}

	raw, err := os.ReadFile(openAPIFile)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the document does not parse: %v", err)
	}

	verbs := map[string]bool{
		"get": true, "post": true, "patch": true, "put": true, "delete": true, "head": true,
	}
	described := map[string]bool{}
	for path, operations := range doc.Paths {
		for method := range operations {
			if verbs[method] {
				described[method+" "+path] = true
			}
		}
	}
	if len(described) == 0 {
		t.Fatal("the document describes no operation, so this test would pass on an empty mux")
	}

	// Both directions. An endpoint served and undescribed is the one somebody
	// calling this without reading the Go finds out about the hard way; an
	// operation described and unserved is the one they trust and get a 404 from.
	if missing := difference(served, described); len(missing) > 0 {
		t.Errorf("served and not described in openapi.yaml: %v", missing)
	}
	if extra := difference(described, served); len(extra) > 0 {
		t.Errorf("described in openapi.yaml and not served: %v", extra)
	}
}

func difference(from, against map[string]bool) []string {
	var out []string
	for key := range from {
		if !against[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}
