package store

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// QueryCounter counts round trips. The ingestion budget is stated per observation and
// asserted, not estimated: at a millisecond of latency, two statements per observation versus
// seven is 200 versus 700 ms of waiting per batch. It is a tracer so nothing in the write path
// knows it is measured. It counts what pgx reports as a query; a COPY is traced through
// another hook, which dropped the figure by a third when two writes moved onto the copy path.
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
