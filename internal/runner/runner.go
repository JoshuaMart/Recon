// Package runner starts a run definition on the platform it is deployed to.
//
// The control plane starts, it never updates. Nothing here modifies a
// definition: the call that does replaces its whole environment map, so it
// would wipe the source API keys the definition carries, and nothing would
// fail. The next run would simply query fewer sources and find less, which is
// the exact failure mode that accounting exists to make visible.
package runner

import (
	"context"
	"log/slog"

	"github.com/JoshuaMart/recon/internal/runs"
)

// None renders a definition and starts nothing.
//
// It is the development shape: the console shows the definition and a person
// runs the image with it. That is the same shape as production minus the call,
// which is what keeps the local path from becoming a second way of starting a
// run.
type None struct{ log *slog.Logger }

// NewNone builds it.
func NewNone(log *slog.Logger) *None { return &None{log: log} }

// Name is what a log line calls this platform.
func (n *None) Name() string { return "none" }

// Start logs what would have been started and returns no identifier.
func (n *None) Start(ctx context.Context, def *runs.Definition) (string, error) {
	n.log.InfoContext(ctx, "run definition ready, nothing starts it here",
		"run", def.RunID, "kind", def.Kind, "scope", def.Scope,
		"targets", def.TargetCount, "args", def.Args)
	return "", nil
}
