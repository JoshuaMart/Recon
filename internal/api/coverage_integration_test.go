//go:build integration

package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/auth"
)

// What the panel reads, and what it deliberately does not.
//
// There is no score. A coverage confidence collapsed into one figure is the
// composite score this console is built without, and the argument is the same
// one: a reader can tell "watched a month, no certificate" from "watched since
// this morning", and a single number cannot.
func TestCoverageStatesTheNumbersAndNeverAScore(t *testing.T) {
	h := newHarness(t)
	token := h.console(t)
	at := time.Now().UTC()

	h.exec(t, `INSERT INTO ct_apex
		(org_id, program_id, apex, watched_since, san_count, wildcard_count, dropped,
		 last_san_at, last_wildcard_at)
		VALUES ($1,$2,'acme.test',$3,42,7,3,$4,$4)`,
		h.org, h.program, at.Add(-2*time.Hour), at.Add(-time.Minute))
	// An apex watched and silent, which is a different finding from an apex
	// nothing is watching and has to read as one.
	h.exec(t, `INSERT INTO ct_apex (org_id, program_id, apex, watched_since)
		VALUES ($1,$2,'quiet.test',$3)`, h.org, h.program, at.Add(-30*time.Minute))
	// The feed was alive for two of those minutes.
	h.exec(t, `INSERT INTO ct_feed_minute (minute, frames) VALUES
		(date_trunc('minute', $1::timestamptz), 1200),
		(date_trunc('minute', $2::timestamptz), 900)`,
		at.Add(-time.Minute), at.Add(-2*time.Minute))

	resp, payload := h.raw(t, http.MethodGet,
		"/programs/"+h.program.String()+"/coverage", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("coverage answered %d: %s", resp.StatusCode, payload)
	}

	body := decode[struct {
		Apexes []struct {
			Apex           string     `json:"apex"`
			WatchedSince   time.Time  `json:"watched_since"`
			Names          int64      `json:"names"`
			Wildcards      int64      `json:"wildcards"`
			Dropped        int64      `json:"dropped"`
			LastName       *time.Time `json:"last_name_at"`
			FeedMinutes    int64      `json:"feed_minutes"`
			WatchedMinutes int64      `json:"watched_minutes"`
		} `json:"apexes"`
	}](t, payload)

	if len(body.Apexes) != 2 {
		t.Fatalf("%d apexes came back, want both", len(body.Apexes))
	}

	first := body.Apexes[0]
	if first.Apex != "acme.test" {
		t.Fatalf("the first apex is %q", first.Apex)
	}
	if first.Names != 42 || first.Wildcards != 7 || first.Dropped != 3 {
		t.Errorf("the counters read %d names, %d wildcards, %d dropped",
			first.Names, first.Wildcards, first.Dropped)
	}
	if first.LastName == nil {
		t.Error("the apex delivered names and says when none of them arrived")
	}
	// The feed's uptime travels beside the counters, or an apex the logs are
	// silent about and a socket that was down read identically, and the second
	// is this deployment's problem rather than a fact about the logs.
	if first.FeedMinutes != 2 {
		t.Errorf("the feed was alive for %d minutes, and two were recorded", first.FeedMinutes)
	}
	if first.WatchedMinutes < 100 {
		t.Errorf("the apex has been watched for %d minutes, and it was seeded two hours ago",
			first.WatchedMinutes)
	}

	// The silent one is present and says so with numbers rather than by being
	// absent from the answer.
	quiet := body.Apexes[1]
	if quiet.Apex != "quiet.test" || quiet.Names != 0 || quiet.LastName != nil {
		t.Errorf("the silent apex reads %+v", quiet)
	}
	if quiet.WatchedMinutes == 0 {
		t.Error("the silent apex says it has been watched for no time at all, which is the " +
			"one reading that makes zero names unreadable")
	}
}

// A programme belonging to somebody else answers 404 rather than 403: a caller
// learning that an identifier exists in another organization is a cross-tenant
// leak of exactly one bit, and one bit is enough to enumerate.
func TestCoverageRefusesAnotherOrganizationsProgramme(t *testing.T) {
	h := newHarness(t)

	other := uuid.New()
	h.exec(t, `INSERT INTO org (id, name) VALUES ($1, 'other')`, other)
	elsewhere := h.token(t, other, auth.ActionReadAssets)

	resp, _ := h.raw(t, http.MethodGet,
		"/programs/"+h.program.String()+"/coverage", elsewhere, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("coverage answered %d to another organization", resp.StatusCode)
	}
}
