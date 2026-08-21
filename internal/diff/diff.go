// Package diff compares two normalized observations and says what changed.
//
// A hash answers "did it change". It does not answer "what changed", and since
// the values are already in the database the hash is worth less than the value
// it stands for: nginx 1.24.0 to 1.25.3 is actionable where a3f9 to 7b21 is
// not.
//
// The constraint is normalization. Comparison runs on the structures the
// single normalization produced, sorted lists and uniform case, or a
// reordering in a response produces a false change, which is exactly the fault
// hashes were supposed to avoid.
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
	// Kind separates a list that gained members from one that lost them and
	// from a value that was replaced, because those read differently and are
	// notified differently.
	Kind   string `json:"kind"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
	// Added and Removed are filled on a list, where naming the members is the
	// whole value of the notification.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// Kinds of change, ordered by how much they say.
const (
	// KindAdded is a pure addition to a list. On a detection list after an
	// instrument update this is a revelation rather than a change in the
	// world, which is what the classification below decides.
	KindAdded = "added"
	// KindRemoved is a pure removal, which is either a detection regression or
	// a real change and is worth a look at low priority.
	KindRemoved = "removed"
	// KindReplaced is a list that both gained and lost, or a scalar that moved.
	KindReplaced = "replaced"
	// KindAppeared is a field that had no value before.
	KindAppeared = "appeared"
	// KindDisappeared is a field that no longer has one.
	KindDisappeared = "disappeared"
)

// listFields are compared as sets of names, because naming what arrived is the
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

// ignored are the fields whose movement says nothing about the target.
//
// The scan counters move with the weather rather than with the service, and a
// diff carrying them would report a change on every pass over a host behind a
// flaky path. They stay in the payload, where the lifecycle reads them.
var ignored = map[string]bool{
	"scan": true,
}

// Compare reads two normalized payloads of the same layer.
//
// It walks the union of the keys, so a field that appeared and one that
// vanished are both changes: reading only the new payload would miss the second
// entirely, and a service that lost its title is a service that changed.
func Compare(before, after map[string]any) []Change {
	if before == nil {
		// No previous observation is not a change. A first contact has its own
		// event, and reporting every field of a new asset as a diff would make
		// onboarding a wall of them.
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

	// Nested objects are compared as a whole. Descending into them would
	// produce a field path nobody asked for, and the ones that matter here are
	// small: a certificate, a network block.
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

// Revelation reports whether a diff is the instrument seeing better rather than
// the world changing.
//
// An asset measured under one version as [nginx] and later as
// [nginx, Grafana, Prometheus] has not changed: the observer did. Untreated,
// the diff reads as an application change and alerts, potentially across a
// whole inventory after one update.
//
// The heuristic is assumed: a real deployment happening on the day of a service
// update is misclassified. The trade converts a large volume of noise into a
// small number of ambiguous cases.
func Revelation(changes []Change, previousVersion, version string) bool {
	if previousVersion == "" || version == "" || previousVersion == version {
		return false
	}
	if len(changes) == 0 {
		return false
	}
	for _, change := range changes {
		// Anything but a pure addition is the world moving. A replacement is a
		// version bump on the target, and a removal is either a regression or
		// a real change, and neither is a revelation.
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
			// A port list decodes as numbers, and naming the port that opened
			// is the whole point of the event it produces.
			out = append(out, strconv.FormatFloat(typed, 'f', -1, 64))
		case map[string]any:
			// A detection is an object carrying its evidence, and a
			// notification wants the thing detected. Rendering the whole
			// structure would put a proof block in somebody's chat.
			out = append(out, named(typed))
		default:
			out = append(out, fmt.Sprintf("%v", value))
		}
	}
	return out
}

// named reads the name out of a structured member, with its version when it
// carries one, and falls back to the whole thing when it carries neither.
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

// equal compares two normalized values.
//
// It goes through the rendered form rather than reflect.DeepEqual because both
// sides come from the same decoder and the same normalization, so the shapes
// are already comparable and the cost is a string on a path that runs once per
// changed field.
func equal(a, b any) bool { return fmt.Sprintf("%#v", a) == fmt.Sprintf("%#v", b) }
