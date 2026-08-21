package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Bootstrap turns the configured webhook into the single row that carries it.
//
// It runs only when exactly one organization exists. A configuration file has
// no way to name a tenant, so that is the only case where the intent is
// unambiguous: beyond it, giving a global URL to one of several organizations
// would leak one tenant's alerts into another's channel.
//
// The row is keyed on the organization and the config marker rather than on the
// URL. Without that marker, changing the configured URL and restarting inserts
// a second active row without disabling the first, and every alert goes out
// twice, one of them to the destination just replaced.
func Bootstrap(ctx context.Context, q *sqlcgen.Queries, url, template, minPriority string, log *slog.Logger) error {
	if url == "" {
		log.InfoContext(ctx, "no configured notification channel")
		return nil
	}

	orgs, err := q.OneOrganization(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read the organizations: %w", err)
	}
	switch len(orgs) {
	case 0:
		log.InfoContext(ctx, "no organization yet, the configured channel waits for one")
		return nil
	case 1:
	default:
		log.WarnContext(ctx, "several organizations exist, so the configured channel is not applied",
			"reason", "a configuration file cannot name a tenant")
		return nil
	}

	params := sqlcgen.UpsertConfigChannelParams{
		ID:          pgUUID(uuid.New()),
		OrgID:       orgs[0],
		Url:         url,
		MinPriority: minPriority,
	}
	if template != "" {
		params.Template = &template
	}
	if err := q.UpsertConfigChannel(ctx, params); err != nil {
		return fmt.Errorf("bootstrap the configured channel: %w", err)
	}

	log.InfoContext(ctx, "configured notification channel",
		"org", uuid.UUID(orgs[0].Bytes), "min_priority", minPriority)
	return nil
}
