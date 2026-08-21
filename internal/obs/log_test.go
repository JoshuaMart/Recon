package obs_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/JoshuaMart/recon/internal/obs"
)

func TestALineCarriesTheContextCorrelationID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := obs.NewLogger(&buf, "info", "json")
	log.InfoContext(obs.WithCorrelation(context.Background(), "run-42"), "started")

	if !strings.Contains(buf.String(), `"correlation_id":"run-42"`) {
		t.Errorf("the line carries no correlation id:\n%s", buf.String())
	}
}

// The attribute has to survive the handler being derived, which is what every
// component does when it takes a logger and adds its own name to it.
func TestTheIDSurvivesADerivedLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := obs.NewLogger(&buf, "info", "json").With("component", "store").WithGroup("db")
	log.InfoContext(obs.WithCorrelation(context.Background(), "run-7"), "connected")

	if !strings.Contains(buf.String(), "run-7") {
		t.Errorf("a derived logger dropped the correlation id:\n%s", buf.String())
	}
}

func TestWithoutAnIDNothingIsAdded(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	obs.NewLogger(&buf, "info", "json").InfoContext(context.Background(), "started")

	if strings.Contains(buf.String(), "correlation_id") {
		t.Errorf("an empty id was reported anyway:\n%s", buf.String())
	}
}
