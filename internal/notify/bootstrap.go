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

// Bootstrap turns the configured webhook into the single row that carries it, only when
// exactly one organization exists: a configuration file cannot name a tenant, and a global
// URL given to one of several would leak alerts across tenants. The row is keyed on the
// organization and the config marker, not the URL: otherwise changing the URL and restarting
// inserts a second active row without disabling the first, and every alert goes out twice,
// one to the replaced destination.
func Bootstrap(ctx context.Context, q *sqlcgen.Queries, url, template, minPriority string, log *slog.Logger) (bool, error) {
	if url == "" {
		log.InfoContext(ctx, "no configured notification channel")
		return true, nil
	}

	orgs, err := q.OneOrganization(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read the organizations: %w", err)
	}
	switch len(orgs) {
	case 0:
		// Not settled. The organization is created outside this process, so a channel
		// bootstrapped only at startup would not exist until an unrelated restart. The
		// notifier retries until there is a tenant.
		log.InfoContext(ctx, "no organization yet, the configured channel waits for one")
		return false, nil
	case 1:
	default:
		log.WarnContext(ctx, "several organizations exist, so the configured channel is not applied",
			"reason", "a configuration file cannot name a tenant")
		return true, nil
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
		return false, fmt.Errorf("bootstrap the configured channel: %w", err)
	}

	log.InfoContext(ctx, "configured notification channel",
		"org", uuid.UUID(orgs[0].Bytes), "min_priority", minPriority)
	return true, nil
}
