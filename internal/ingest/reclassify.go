package ingest

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Reclassification is what a scope change moved, so the effect is readable at once.
type Reclassification struct {
	Examined int
	Moved    int
	Gained   int
	Lost     int
	Changes  []Change
}

// Change is one asset that moved.
type Change struct {
	AssetID uuid.UUID
	Key     string
	From    scope.Status
	To      scope.Status
}

// Reclassify re-evaluates a whole program against a rule set, in the transaction that writes
// the rule: a rule in force whose consequence the inventory lacks is a perimeter that lies
// (lost coverage or a scan outside authorization), and afterwards leaves a window scanning
// what was just taken away. The price is examining every asset of the program, so a rule
// change on hundreds of thousands is a long query and transaction; bounded by being per
// programme, not tenant, and by returning what moved.
func (i *Ingestor) Reclassify(
	ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID, set *scope.Set, due Schedule,
) (Reclassification, error) {
	rows, err := q.ListProgramAssets(ctx, sqlcgen.ListProgramAssetsParams{
		ProgramID: uuidTo(programID),
	})
	if err != nil {
		return Reclassification{}, fmt.Errorf("list assets: %w", err)
	}

	out := Reclassification{Examined: len(rows)}

	for _, row := range rows {
		key := normalize.Key{Kind: normalize.Kind(row.Kind), Value: row.Key}
		if row.Host != nil {
			key.Host = *row.Host
		}
		// A row written before the host column existed has none, so classification falls
		// back to the key. It repairs on the next observation, not a catch-up migration
		// that would reimplement key parsing in SQL.
		if key.Host == "" {
			key.Host = row.Key
		}

		was := scope.Status(row.ScopeStatus)
		now := set.Classify(scope.Target{Key: key, Addresses: resolved(row)})
		if now == was {
			continue
		}

		params := sqlcgen.ApplyScopeStatusParams{
			AssetID:     row.ID,
			ScopeStatus: string(now),
		}
		if now == scope.InScope {
			params.NextResolveAt = stampPtr(due.Resolve)
			params.NextFullAt = stampPtr(due.Full)
		}
		if err := q.ApplyScopeStatus(ctx, params); err != nil {
			return out, fmt.Errorf("reclassify %s: %w", row.Key, err)
		}

		out.Moved++
		switch {
		case now == scope.InScope:
			out.Gained++
		case was == scope.InScope:
			out.Lost++
		}
		out.Changes = append(out.Changes, Change{
			AssetID: uuid.UUID(row.ID.Bytes), Key: row.Key, From: was, To: now,
		})
	}

	return out, nil
}

// resolved is what a CIDR rule reads. The address lives on the projection, so the
// walk joins it in; skipping the join cost far more than it saved: without an
// address a CIDR exclusion stops matching and the excluded asset returns in scope
// with its due dates re-armed.
func resolved(row sqlcgen.ListProgramAssetsRow) []netip.Addr {
	if !row.Ip.IsValid() {
		return nil
	}
	return []netip.Addr{row.Ip}
}

// DefaultSchedule is when a freshly scheduled asset becomes due. A hand-entered
// host is due for full, not resolution: the person typed it to learn what it
// exposes.
func DefaultSchedule(at time.Time, manual bool) Schedule {
	if manual {
		return Schedule{Resolve: &at, Full: &at}
	}
	return Schedule{Resolve: &at}
}
