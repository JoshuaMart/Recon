package render

import (
	"context"
	"time"
)

// Loop runs the pass on a tick.
//
// It holds no state between two of them, which is the whole reason the render
// path needs no recovery mechanism: a pass that dies mid flight leaves every
// due date exactly where it found it.
type Loop struct {
	pass     *Pass
	interval time.Duration
}

// NewLoop builds the tick.
func NewLoop(pass *Pass, interval time.Duration) *Loop {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Loop{pass: pass, interval: interval}
}

// Run ticks until the context ends.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			summary, err := l.pass.Once(ctx)
			if err != nil {
				l.pass.log.ErrorContext(ctx, "render pass failed", "error", err)
				continue
			}
			if summary.Selected == 0 {
				continue
			}
			l.pass.log.InfoContext(ctx, "render pass",
				"selected", summary.Selected, "rendered", summary.Rendered,
				"pages", summary.Pages, "refused", summary.Refused,
				"skipped", summary.Skipped, "failed", summary.Failed,
				"saturated", summary.Saturated, "starved", summary.Starved)
		}
	}
}
