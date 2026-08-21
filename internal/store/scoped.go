package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Scoped is the application pool, and the only way to reach it.
//
// It exposes one method, and that is the design rather than an economy. The
// policies read a transaction-scoped variable, so a query outside a transaction
// cannot carry one; an API offering both shapes is an API where the wrong one
// gets used eventually, and the failure is zero rows rather than an error.
//
// The role a process connects with is chosen when a pool is opened. This type
// is what makes the second half true as well: a caller holding it cannot ask
// for a connection without saying which organization it is asking for.
type Scoped struct{ pool *pgxpool.Pool }

// NewScoped wraps the application pool.
func NewScoped(pool *pgxpool.Pool) *Scoped { return &Scoped{pool: pool} }

// ErrNoOrganization is a caller that reached the database without naming a
// tenant.
//
// Refused here rather than passed through. An empty organization would set the
// variable to nothing, every policy would match no row, and the request would
// answer an empty inventory with a 200. That is the silent failure this whole
// mechanism exists to remove, so it becomes an error at the only place that can
// still tell the difference.
var ErrNoOrganization = errors.New("no organization was named for this transaction")

// Begin opens a transaction that carries an organization.
//
// The variable is set at the start of the transaction carrying the query and
// never when the connection is acquired. SET LOCAL is transaction scoped and
// disappears at commit, which is exactly the property wanted: a plain SET at
// acquisition survives the connection going back to the pool, and the next
// query from another tenant inherits the previous context. That would be the
// cross-tenant leak row-level security exists to prevent, introduced by the
// mechanism meant to prevent it.
//
// The caller rolls back or commits as usual. A rollback discards the setting
// with everything else, which is the point.
func (s *Scoped) Begin(ctx context.Context, org uuid.UUID) (pgx.Tx, error) {
	if org == uuid.Nil {
		return nil, ErrNoOrganization
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}

	// set_config rather than SET LOCAL, because SET takes no bind parameter and
	// an organization interpolated into a statement is an organization that can
	// carry something other than a uuid. The third argument is is_local.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, org.String()); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("scope the transaction to %s: %w", org, err)
	}
	return tx, nil
}

// Ping is what a readiness probe asks. It reads no table, so it needs no
// organization.
func (s *Scoped) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
