// Package runner starts a run definition on the platform it is deployed to. The
// control plane starts, it never updates: the update call replaces the whole
// environment map, wiping the source API keys without any error, and later runs
// would just query fewer sources.
package runner

import (
	"context"
	"log/slog"

	"github.com/JoshuaMart/recon/internal/runs"
)

// None renders a definition and starts nothing: the development shape, where a
// person runs the image with the console's definition. It is production minus the
// call, so the local path is not a second way of starting a run.
type None struct{ log *slog.Logger }

// NewNone builds it.
func NewNone(log *slog.Logger) *None { return &None{log: log} }

// Name is what a log line calls this platform.
func (n *None) Name() string { return "none" }

// Start logs what would have been started and returns no identifier. The
// invocation is redacted: it carries two live bearer credentials (report posting
// and target fetching), and at INFO they would land in every log, which outlives
// the run.
func (n *None) Start(ctx context.Context, def *runs.Definition) (string, error) {
	n.log.InfoContext(ctx, "run definition ready, nothing starts it here",
		"run", def.RunID, "kind", def.Kind, "scope", def.Scope,
		"targets", def.TargetCount, "args", runs.Redacted(def.Args))
	return "", nil
}

// Status has nothing to read: Start named no execution, so nothing calls it.
func (n *None) Status(context.Context, string) (runs.Execution, error) {
	return runs.Execution{}, nil
}

// Stop has nothing to stop, for the same reason.
func (n *None) Stop(context.Context, string) error { return nil }
