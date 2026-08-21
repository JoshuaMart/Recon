// Package obs holds logging and, later, metrics.
//
// Logs are structured and carry a correlation id end to end. That is here from
// the start rather than added when it is first missed: grafting correlation
// onto an existing pipeline means touching every write path.
package obs

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// correlationKey is unexported so that nothing outside this package can put a
// value under it by accident.
type correlationKey struct{}

// WithCorrelation returns a context carrying an id that every log line written
// from it will report.
func WithCorrelation(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationKey{}, id)
}

// Correlation reads the id back, empty when there is none.
func Correlation(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// NewLogger builds the process logger.
//
// It writes to the given writer, which is stderr everywhere in production:
// stdout is reserved so that a document a machine consumes stays parseable.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(&correlated{Handler: handler})
}

// correlated attaches the context's correlation id to every record, so that no
// call site has to remember to pass it.
type correlated struct{ slog.Handler }

func (c *correlated) Handle(ctx context.Context, record slog.Record) error {
	if id := Correlation(ctx); id != "" {
		record.AddAttrs(slog.String("correlation_id", id))
	}
	return c.Handler.Handle(ctx, record)
}

func (c *correlated) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &correlated{Handler: c.Handler.WithAttrs(attrs)}
}

func (c *correlated) WithGroup(name string) slog.Handler {
	return &correlated{Handler: c.Handler.WithGroup(name)}
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
