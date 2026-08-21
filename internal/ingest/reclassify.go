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

// Reclassification is what a scope change moved, so the effect is readable
// immediately rather than counted later.
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

// Reclassify re-evaluates a whole program against a rule set.
//
// It runs in the transaction that writes the rule. A rule in force whose
// consequence the inventory does not carry is a perimeter that lies, and the
// two directions of that lie are lost coverage and a scan outside the
// authorization. Doing it afterwards leaves a window where the system scans
// what was just taken away from it.
//
// The price, named rather than discovered: this examines every asset of the
// program. On one holding hundreds of thousands, a rule change is a long query
// and a long transaction. Two things bound it. The scope is the programme
// rather than the tenant, and what comes back says what moved.
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
		// A row written before the host column existed has none, and the
		// classification falls back to the key. It repairs itself on the next
		// observation rather than through a catch-up migration that would
		// reimplement key parsing in SQL.
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

// resolved is where a CIDR rule would read an address from. The walk does not
// carry one today: an address lives on the projection rather than on the
// identity, and reading it would cost a join on a pass that already visits
// every asset of a programme.
func resolved(sqlcgen.ListProgramAssetsRow) []netip.Addr { return nil }

// DefaultSchedule is when a freshly scheduled asset becomes due.
//
// A hand-entered host is due for a full run rather than a resolution: somebody
// typed it in to find out what it exposes, and a resolution would only report
// that the name answers.
func DefaultSchedule(at time.Time, manual bool) Schedule {
	if manual {
		return Schedule{Resolve: &at, Full: &at}
	}
	return Schedule{Resolve: &at}
}
