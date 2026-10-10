package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Scoped is the application pool and the only way to reach it. It exposes one
// method on purpose: the policies read a transaction-scoped variable, so a query
// outside a transaction cannot carry one, and an API offering both shapes gets
// the wrong one used eventually, with zero rows rather than an error.
//
// A caller holding this cannot get a connection without naming an organization.
type Scoped struct{ pool *pgxpool.Pool }

// NewScoped wraps the application pool.
func NewScoped(pool *pgxpool.Pool) *Scoped { return &Scoped{pool: pool} }

// ErrNoOrganization is a caller that reached the database without naming a
// tenant. Refused here: an empty organization would set the variable to nothing,
// every policy would match no row, and the request would answer an empty
// inventory with a 200, the silent failure this mechanism exists to remove.
var ErrNoOrganization = errors.New("no organization was named for this transaction")

// Begin opens a transaction that carries an organization. The variable is set at
// the start of the transaction, never at connection acquisition: SET LOCAL
// disappears at commit, while a plain SET would survive the connection returning
// to the pool and leak the previous tenant's context to the next query, the
// cross-tenant leak row-level security exists to prevent.
//
// The caller rolls back or commits as usual; a rollback discards the setting.
func (s *Scoped) Begin(ctx context.Context, org uuid.UUID) (pgx.Tx, error) {
	if org == uuid.Nil {
		return nil, ErrNoOrganization
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}

	// set_config rather than SET LOCAL: SET takes no bind parameter, and an
	// interpolated organization could carry something other than a uuid. The third
	// argument is is_local.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, org.String()); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("scope the transaction to %s: %w", org, err)
	}
	return tx, nil
}

// Ping is what a readiness probe asks. It reads no table, so needs no organization.
func (s *Scoped) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
