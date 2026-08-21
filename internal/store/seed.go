package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
	"github.com/JoshuaMart/recon/seeds"
)

// pivotEntry is one line of the denylist file.
type pivotEntry struct {
	Pattern string `yaml:"pattern"`
	Note    string `yaml:"note"`
}

// Seed applies the repository's reference data.
//
// It runs on every deployment rather than once, because these lists grow as new
// frameworks and edges appear and a migration per addition is a migration
// nobody wants to write. It replaces rather than merges: a merge would let an
// entry added outside the repository survive every deployment, and the
// divergence would be invisible.
//
// It runs as the owner. The application role has no write on this table at all,
// so an entry can only come from here.
func Seed(ctx context.Context, conn *pgx.Conn) (int, error) {
	var file map[string][]pivotEntry
	if err := yaml.Unmarshal(seeds.GenericPivots, &file); err != nil {
		return 0, fmt.Errorf("read the generic pivot list: %w", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if err := q.ClearGenericPivots(ctx); err != nil {
		return 0, fmt.Errorf("clear the generic pivot list: %w", err)
	}

	written := 0
	for pivotType, entries := range file {
		for _, entry := range entries {
			if entry.Pattern == "" {
				return 0, fmt.Errorf("generic pivot list: an entry of %q has no pattern", pivotType)
			}
			params := sqlcgen.InsertGenericPivotParams{
				PivotType: pivotType,
				Pattern:   entry.Pattern,
			}
			if entry.Note != "" {
				note := entry.Note
				params.Note = &note
			}
			if err := q.InsertGenericPivot(ctx, params); err != nil {
				return 0, fmt.Errorf("seed %s/%s: %w", pivotType, entry.Pattern, err)
			}
			written++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return written, nil
}
