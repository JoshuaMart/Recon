// Package diff compares two normalized observations and says what changed. A hash says
// "did it change", not "what": nginx 1.24.0 to 1.25.3 is actionable where a3f9 to 7b21 is
// not. Comparison runs on the single normalization's output (sorted lists, uniform case),
// or a reordered response is a false change, the fault hashes were meant to avoid.
package diff

import (
	"fmt"
	"sort"
	"strconv"
	str "strings"
)

// Change is one field that moved.
type Change struct {
	Field string `json:"field"`
	// Kind separates a list that gained members, one that lost them, and a replaced
	// value, since they read and notify differently.
	Kind   string `json:"kind"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
	// Added and Removed are filled on a list, where naming the members is the value.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// Kinds of change, ordered by how much they say.
const (
	// KindAdded is a pure addition to a list. After an instrument update on a
	// detection list it is a revelation, not a change in the world.
	KindAdded = "added"
	// KindRemoved is a pure removal: a detection regression or a real change, worth
	// a low-priority look.
	KindRemoved = "removed"
	// KindReplaced is a list that both gained and lost, or a scalar that moved.
	KindReplaced = "replaced"
	// KindAppeared is a field that had no value before.
	KindAppeared = "appeared"
	// KindDisappeared is a field that no longer has one.
	KindDisappeared = "disappeared"
)

// listFields are compared as sets of names; naming what arrived is the
// difference between a useful notification and a boolean.
var listFields = map[string]bool{
	"tech":           true,
	"technologies":   true,
	"cookie_names":   true,
	"external_hosts": true,
	"web_sockets":    true,
	"open_ports":     true,
	"redirects":      true,
	"cname":          true,
	"addresses":      true,
}

// ignored are the fields whose movement says nothing about the target. Scan
// counters move with the weather, and a diff carrying them would report a change
// on every pass over a host behind a flaky path. They stay in the payload for the
// lifecycle.
var ignored = map[string]bool{
	"scan": true,
}

// Compare reads two normalized payloads of the same layer. It walks the union of
// the keys: a service that lost its title changed, and reading only the new
// payload would miss it.
func Compare(before, after map[string]any) []Change {
	if before == nil {
		// No previous observation is not a change: first contact has its own event, and
		// diffing every field of a new asset would make onboarding a wall of them.
		return nil
	}

	keys := map[string]struct{}{}
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}

	names := make([]string, 0, len(keys))
	for key := range keys {
		if !ignored[key] {
			names = append(names, key)
		}
	}
	sort.Strings(names)

	changes := make([]Change, 0, 4)
	for _, name := range names {
		if change, moved := compareField(name, before[name], after[name]); moved {
			changes = append(changes, change)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	return changes
}

func compareField(name string, before, after any) (Change, bool) {
	switch {
	case before == nil && after == nil:
		return Change{}, false
	case before == nil:
		return Change{Field: name, Kind: KindAppeared, After: after}, true
	case after == nil:
		return Change{Field: name, Kind: KindDisappeared, Before: before}, true
	}

	if listFields[name] {
		return compareList(name, before, after)
	}

	// Nested objects compare whole. Descending would produce a field path nobody
	// asked for, and the ones that matter (a certificate, a network block) are small.
	if equal(before, after) {
		return Change{}, false
	}
	return Change{Field: name, Kind: KindReplaced, Before: before, After: after}, true
}

func compareList(name string, before, after any) (Change, bool) {
	was, is := strings(before), strings(after)

	seen := map[string]struct{}{}
	for _, value := range was {
		seen[value] = struct{}{}
	}
	now := map[string]struct{}{}
	for _, value := range is {
		now[value] = struct{}{}
	}

	var added, removed []string
	for _, value := range is {
		if _, held := seen[value]; !held {
			added = append(added, value)
		}
	}
	for _, value := range was {
		if _, held := now[value]; !held {
			removed = append(removed, value)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return Change{}, false
	}

	change := Change{Field: name, Added: added, Removed: removed}
	switch {
	case len(removed) == 0:
		change.Kind = KindAdded
	case len(added) == 0:
		change.Kind = KindRemoved
	default:
		change.Kind = KindReplaced
	}
	return change, true
}

// Revelation reports whether a diff is the instrument seeing better rather than the world
// changing: an asset measured as [nginx] and later [nginx, Grafana, Prometheus] has not
// changed, the observer did, and one update would otherwise alert across a whole inventory.
// A heuristic: a real deployment on the day of a service update is misclassified, trading a
// large volume of noise for a few ambiguous cases.
func Revelation(changes []Change, previousVersion, version string) bool {
	if previousVersion == "" || version == "" || previousVersion == version {
		return false
	}
	if len(changes) == 0 {
		return false
	}
	for _, change := range changes {
		// Anything but a pure addition is the world moving: a replacement is a version
		// bump on the target, a removal a regression or real change.
		if change.Kind != KindAdded && change.Kind != KindAppeared {
			return false
		}
	}
	return true
}

// Summarise renders a diff as the line a notification leads with.
func Summarise(changes []Change) string {
	if len(changes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(changes))
	for _, change := range changes {
		if change.Field == "chain" {
			parts = append(parts, "chain: "+chainLine(change.Before, change.After))
			continue
		}
		switch change.Kind {
		case KindAdded:
			parts = append(parts, change.Field+": +"+str.Join(change.Added, ", "))
		case KindRemoved:
			parts = append(parts, change.Field+": -"+str.Join(change.Removed, ", "))
		case KindReplaced:
			if len(change.Added) > 0 || len(change.Removed) > 0 {
				parts = append(parts, fmt.Sprintf("%s: +%s -%s", change.Field,
					str.Join(change.Added, ", "), str.Join(change.Removed, ", ")))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s: %v → %v", change.Field, change.Before, change.After))
		case KindAppeared:
			parts = append(parts, fmt.Sprintf("%s: → %v", change.Field, change.After))
		case KindDisappeared:
			parts = append(parts, fmt.Sprintf("%s: %v →", change.Field, change.Before))
		}
	}
	return str.Join(parts, "; ")
}

// strings reads a normalized list, whatever the decoder made of its members.
func strings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, value := range list {
		switch typed := value.(type) {
		case string:
			out = append(out, typed)
		case float64:
			// A port list decodes as numbers, and the event exists to name the port that
			// opened.
			out = append(out, strconv.FormatFloat(typed, 'f', -1, 64))
		case map[string]any:
			// A detection is an object carrying its evidence, and a notification wants the
			// thing detected, not a proof block in someone's chat.
			out = append(out, named(typed))
		default:
			out = append(out, fmt.Sprintf("%v", value))
		}
	}
	return out
}

// named reads the name from a structured member, with its version if present,
// else the whole thing.
func named(value map[string]any) string {
	name, ok := value["name"].(string)
	if !ok || name == "" {
		return fmt.Sprintf("%v", value)
	}
	if version, ok := value["version"].(string); ok && version != "" {
		return name + " " + version
	}
	return name
}

// equal compares two normalized values through the rendered form, not
// reflect.DeepEqual: both sides share a decoder and normalization, so shapes are
// comparable, and the cost is a string once per changed field.
func equal(a, b any) bool { return fmt.Sprintf("%#v", a) == fmt.Sprintf("%#v", b) }

// hop is the part of a redirect hop a reader follows: where it went and what answered.
type hop struct {
	url     string
	status  int
	title   string
	headers map[string]any
}

func hops(v any) []hop {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]hop, 0, len(list))
	for _, entry := range list {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		h := hop{}
		h.url, _ = fields["url"].(string)
		h.title, _ = fields["title"].(string)
		h.headers, _ = fields["headers"].(map[string]any)
		// Through float64: the payload has been through JSON.
		if code, ok := fields["status_code"].(float64); ok {
			h.status = int(code)
		}
		out = append(out, h)
	}
	return out
}

// path renders a chain as its URLs and statuses, the sequence a person reads.
func path(chain []hop) string {
	steps := make([]string, 0, len(chain))
	for _, h := range chain {
		steps = append(steps, fmt.Sprintf("%s (%d)", h.url, h.status))
	}
	return str.Join(steps, " → ")
}

// chainLine says what moved in a redirect chain. Printed whole, a chain carries every
// header of every hop: one Content-Security-Policy made a 7 000 character notification
// that Discord refused, and that nobody could have read.
func chainLine(before, after any) string {
	was, is := hops(before), hops(after)
	switch {
	case len(was) == 0 && len(is) == 0:
		return "changed"
	case len(was) == 0:
		return path(is)
	case len(is) == 0:
		return path(was) + ", now none"
	}
	if from, to := path(was), path(is); from != to {
		return to + ", was " + from
	}

	// Same path, so a hop's content moved: name the headers, never their values.
	moved := map[string]struct{}{}
	titles := false
	for i := range is {
		if was[i].title != is[i].title {
			titles = true
		}
		for name, value := range is[i].headers {
			if !equal(was[i].headers[name], value) {
				moved[name] = struct{}{}
			}
		}
		for name := range was[i].headers {
			if _, kept := is[i].headers[name]; !kept {
				moved[name] = struct{}{}
			}
		}
	}
	parts := make([]string, 0, 2)
	if len(moved) > 0 {
		names := make([]string, 0, len(moved))
		for name := range moved {
			names = append(names, name)
		}
		sort.Strings(names)
		parts = append(parts, "headers changed ("+str.Join(names, ", ")+")")
	}
	if titles {
		parts = append(parts, "title changed")
	}
	if len(parts) == 0 {
		return "changed"
	}
	return str.Join(parts, ", ") + " on " + path(is)
}
