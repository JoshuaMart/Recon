package api

import (
	"testing"
	"time"
)

func TestChangeCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 23, 17, 27, 8, 569716000, time.UTC)
	got, id, ok := parseChangeCursor(changeCursor(at, 42))
	if !ok || !got.Equal(at) || id != 42 {
		t.Fatalf("round trip = %v %d %v", got, id, ok)
	}
	for _, bad := range []string{"", "nope", "MjAyNg"} {
		if _, _, ok := parseChangeCursor(bad); ok {
			t.Errorf("%q decoded", bad)
		}
	}
}
