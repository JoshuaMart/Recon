package store

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// QueryCounter counts round trips.
//
// The ingestion budget is stated per observation and asserted rather than
// estimated, because the number is what keeps a first asset reaching the
// database quickly: at a millisecond of latency, the difference between two
// statements per observation and seven is the difference between a batch
// costing two hundred milliseconds of waiting and seven hundred.
//
// It is a tracer rather than a wrapper so that nothing in the write path has to
// know it is being measured.
type QueryCounter struct{ n atomic.Int64 }

// TraceQueryStart counts one.
func (c *QueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

// TraceQueryEnd completes the interface and does nothing.
func (c *QueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Count is how many statements have been sent.
func (c *QueryCounter) Count() int64 { return c.n.Load() }

// Reset starts a fresh measurement.
func (c *QueryCounter) Reset() { c.n.Store(0) }
