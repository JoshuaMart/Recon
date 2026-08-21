package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/auth"
)

const key = "a-signing-key-long-enough-to-be-one"

func signer(t *testing.T) *auth.Signer {
	t.Helper()

	s, err := auth.NewSigner(key)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return s
}

// A short key is not a weak configuration, it is an unsigned one.
func TestAKeyTooShortIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := auth.NewSigner("short"); err == nil {
		t.Error("a five byte signing key was accepted")
	}
}

func TestAMintedTokenVerifies(t *testing.T) {
	t.Parallel()

	run := uuid.New()
	token := signer(t).Mint(auth.PurposeReport, run, time.Now().Add(time.Hour))

	got, err := signer(t).Verify(auth.PurposeReport, token, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != run {
		t.Errorf("run = %s, want %s", got, run)
	}
}

// The purpose is inside the signature, so a token minted to fetch a target list
// cannot be replayed to post a report.
func TestATokenDoesNotCrossPurposes(t *testing.T) {
	t.Parallel()

	token := signer(t).Mint(auth.PurposeTargets, uuid.New(), time.Now().Add(time.Hour))

	if _, err := signer(t).Verify(auth.PurposeReport, token, time.Now()); err == nil {
		t.Error("a targets token was accepted to post a report")
	}
}

func TestAnExpiredTokenIsRefusedAsSuch(t *testing.T) {
	t.Parallel()

	token := signer(t).Mint(auth.PurposeReport, uuid.New(), time.Now().Add(-time.Minute))

	_, err := signer(t).Verify(auth.PurposeReport, token, time.Now())
	if !errors.Is(err, auth.ErrExpired) {
		t.Errorf("err = %v, want an expiry: it is intrinsic, which is why there is "+
			"nothing to store and nothing to purge", err)
	}
}

func TestATamperedTokenIsRefused(t *testing.T) {
	t.Parallel()

	run := uuid.New()
	token := signer(t).Mint(auth.PurposeReport, run, time.Now().Add(time.Hour))

	// The whole point of the signature: naming another run has to fail.
	forged := strings.Replace(token, run.String(), uuid.New().String(), 1)
	if _, err := signer(t).Verify(auth.PurposeReport, forged, time.Now()); err == nil {
		t.Error("a token naming another run was accepted")
	}

	// And so does a signature from another key.
	other, err := auth.NewSigner("a-completely-different-signing-key-x")
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	if _, err := signer(t).Verify(auth.PurposeReport,
		other.Mint(auth.PurposeReport, run, time.Now().Add(time.Hour)), time.Now()); err == nil {
		t.Error("a token signed with another key was accepted")
	}
}

func TestAnEmptyCredentialIsItsOwnAnswer(t *testing.T) {
	t.Parallel()

	if _, err := signer(t).Verify(auth.PurposeReport, "  ", time.Now()); !errors.Is(err, auth.ErrMissing) {
		t.Errorf("err = %v, want a missing credential", err)
	}
}

// A run holds exactly one action. A scanner that could also schedule work would
// be able to spend a programme's budget on targets it chose.
func TestAPrincipalHoldsOnlyWhatItWasGiven(t *testing.T) {
	t.Parallel()

	p := auth.Principal{Actions: []auth.Action{auth.ActionIngest}}

	if !p.Can(auth.ActionIngest) {
		t.Error("the principal cannot do what it was given")
	}
	for _, action := range []auth.Action{auth.ActionReadAssets, auth.ActionManageScope, auth.ActionManageJobs} {
		if p.Can(action) {
			t.Errorf("a run can %s, and everything it needs is already in its definition", action)
		}
	}
}
