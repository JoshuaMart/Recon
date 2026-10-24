package runs

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTokensBindRunPurposeAndExpiry(t *testing.T) {
	tk := NewTokens("0123456789abcdef0123456789abcdef")
	run, other := uuid.New(), uuid.New()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tok := tk.Mint(PurposeReport, run, now.Add(time.Hour))

	if err := tk.Check(tok, PurposeReport, run, now); err != nil {
		t.Fatalf("a valid token refused: %v", err)
	}
	for name, err := range map[string]error{
		"another purpose": tk.Check(tok, PurposeTargets, run, now),
		"another run":     tk.Check(tok, PurposeReport, other, now),
		"expired":         tk.Check(tok, PurposeReport, run, now.Add(time.Hour)),
		"another key":     NewTokens("another key, also long enough!!").Check(tok, PurposeReport, run, now),
		"tampered":        tk.Check(strings.Replace(tok, run.String(), other.String(), 1), PurposeReport, other, now),
		"empty":           tk.Check("", PurposeReport, run, now),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
