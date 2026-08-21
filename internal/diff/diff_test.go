package diff_test

import (
	"encoding/json"
	"testing"

	"github.com/JoshuaMart/recon/internal/diff"
)

func payload(t *testing.T, raw string) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// A hash answers "did it change". The values are already in the database, so
// the hash is worth less than the value it stands for.
func TestADiffNamesWhatMoved(t *testing.T) {
	t.Parallel()

	changes := diff.Compare(
		payload(t, `{"status_code":200,"title":"Home","tech":["PHP 8.2","nginx 1.24.0"]}`),
		payload(t, `{"status_code":302,"title":"Home","tech":["PHP 8.3","nginx 1.24.0"]}`),
	)

	if len(changes) != 2 {
		t.Fatalf("%d fields moved: %+v", len(changes), changes)
	}
	line := diff.Summarise(changes)
	for _, want := range []string{"status_code: 200 → 302", "PHP 8.3", "PHP 8.2"} {
		if !contains(line, want) {
			t.Errorf("the line %q does not carry %q", line, want)
		}
	}
	// A field that did not move is not in the diff, which is what keeps a
	// notification readable.
	if contains(line, "title") {
		t.Errorf("an unchanged field is in the diff: %q", line)
	}
}

// A reordering is not a change. It is the fault hashes were supposed to avoid
// and the reason comparison runs on normalized structures.
func TestAReorderingIsNotAChange(t *testing.T) {
	t.Parallel()

	changes := diff.Compare(
		payload(t, `{"tech":["nginx","PHP"],"external_hosts":["a.test","b.test"]}`),
		payload(t, `{"tech":["PHP","nginx"],"external_hosts":["b.test","a.test"]}`),
	)
	if len(changes) != 0 {
		t.Fatalf("a reordering produced %+v", changes)
	}
}

// A first contact has its own event. Reporting every field of a new asset as a
// diff would make onboarding a wall of them.
func TestNoPreviousObservationIsNotADiff(t *testing.T) {
	t.Parallel()

	if changes := diff.Compare(nil, payload(t, `{"status_code":200}`)); changes != nil {
		t.Fatalf("a first observation produced %+v", changes)
	}
}

// A field that vanished is a change, and reading only the new payload would
// miss it entirely.
func TestAFieldThatVanishedIsAChange(t *testing.T) {
	t.Parallel()

	changes := diff.Compare(
		payload(t, `{"status_code":200,"title":"Admin"}`),
		payload(t, `{"status_code":200}`),
	)
	if len(changes) != 1 || changes[0].Kind != diff.KindDisappeared || changes[0].Field != "title" {
		t.Fatalf("the diff reads %+v", changes)
	}
}

// The instrument is dated. An asset measured as [nginx] and later as
// [nginx, Grafana] has not changed: the observer did, and alerting on it after
// one update would alert across a whole inventory.
func TestAPureAdditionAfterAVersionBumpIsARevelation(t *testing.T) {
	t.Parallel()

	revealed := diff.Compare(
		payload(t, `{"technologies":["nginx"]}`),
		payload(t, `{"technologies":["Grafana","Prometheus","nginx"]}`),
	)
	if !diff.Revelation(revealed, "2.1.0", "2.2.0") {
		t.Fatal("a pure addition across a version bump read as a real change")
	}

	// The same addition without a version bump is the world moving.
	if diff.Revelation(revealed, "2.1.0", "2.1.0") {
		t.Fatal("an addition under one version read as the instrument improving")
	}
	// And a replacement is a real change whatever the version did.
	replaced := diff.Compare(
		payload(t, `{"technologies":["nginx 1.24"]}`),
		payload(t, `{"technologies":["nginx 1.25"]}`),
	)
	if diff.Revelation(replaced, "2.1.0", "2.2.0") {
		t.Fatal("a version bump on the target read as the instrument improving")
	}
	// A removal is a regression or a real change, and neither is a revelation.
	removed := diff.Compare(
		payload(t, `{"technologies":["nginx","PHP"]}`),
		payload(t, `{"technologies":["nginx"]}`),
	)
	if diff.Revelation(removed, "2.1.0", "2.2.0") {
		t.Fatal("a removal read as the instrument improving")
	}
}

// A port list decodes as numbers, and naming the port that opened is the whole
// point of the event it produces.
func TestAnOpenedPortIsNamed(t *testing.T) {
	t.Parallel()

	changes := diff.Compare(
		payload(t, `{"open_ports":[443]}`),
		payload(t, `{"open_ports":[443,8090]}`),
	)
	if len(changes) != 1 || changes[0].Kind != diff.KindAdded {
		t.Fatalf("the diff reads %+v", changes)
	}
	if len(changes[0].Added) != 1 || changes[0].Added[0] != "8090" {
		t.Fatalf("the port that opened is %v", changes[0].Added)
	}
}

// The sweep counters move with the weather rather than with the service.
func TestTheSweepCountersAreNotADiff(t *testing.T) {
	t.Parallel()

	changes := diff.Compare(
		payload(t, `{"open_ports":[443],"scan":{"scanned":32,"open":1,"refused":30,"filtered":1}}`),
		payload(t, `{"open_ports":[443],"scan":{"scanned":32,"open":1,"refused":31,"filtered":0}}`),
	)
	if len(changes) != 0 {
		t.Fatalf("a flaky path produced %+v", changes)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && index(haystack, needle) >= 0)
}

func index(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
