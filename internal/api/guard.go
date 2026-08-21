package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Guard resolves a console credential into a principal, once, in one place.
//
// Checks scattered inline would force an audit of every endpoint the day a
// second role appears, and that audit is the thing nobody does.
type Guard struct {
	// system is the pool that crosses tenants, and it is the right one here
	// for a reason that is not convenience: a token names itself and never an
	// organization, so this is the query that *discovers* the tenant and
	// cannot be filtered by one. Leaving it subject to the policies would
	// offer two ways out, a predicate nobody can satisfy or a table quietly
	// exempted, and it is the second that gets chosen under pressure.
	//
	// It is one statement, keyed by a hash the caller has to hold already, and
	// it returns one row. Everything after it runs scoped.
	system *pgxpool.Pool
	now    func() time.Time
	log    *slog.Logger
}

// NewGuard builds the authorization layer.
func NewGuard(system *pgxpool.Pool, log *slog.Logger) *Guard {
	return &Guard{system: system, now: time.Now, log: log}
}

// Handler is a route that has already been told who is asking.
type Handler func(http.ResponseWriter, *http.Request, auth.Principal)

// Require resolves the caller and refuses one that does not hold the action.
func (g *Guard) Require(action auth.Action, next Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := g.resolve(r)
		if err != nil {
			// One answer to the outside for a missing, wrong or expired
			// credential. Which one it was belongs in a log, not in a reply.
			g.log.WarnContext(r.Context(), "request refused", "path", r.URL.Path, "reason", err)
			fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
			return
		}
		if !principal.Can(action) {
			// Distinct from the above on purpose: a caller who authenticated
			// and lacks a privilege has a different problem from one whose
			// credential is wrong, and telling them apart is what makes the
			// separation of actions usable rather than mysterious.
			fail(w, http.StatusForbidden, "forbidden", "this credential does not hold "+string(action))
			return
		}
		next(w, r, principal)
	})
}

func (g *Guard) resolve(r *http.Request) (auth.Principal, error) {
	header := r.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found || strings.TrimSpace(token) == "" {
		return auth.Principal{}, auth.ErrMissing
	}

	row, err := sqlcgen.New(g.system).PrincipalForToken(r.Context(), sqlcgen.PrincipalForTokenParams{
		TokenHash: auth.Hash(token),
		At:        stamp(g.now()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, auth.ErrInvalid
	}
	if err != nil {
		return auth.Principal{}, err
	}

	principal := auth.Principal{
		OrgID:   uuid.UUID(row.OrgID.Bytes),
		Actions: auth.Actions(row.Scopes),
	}
	if row.CreatedBy.Valid {
		actor := uuid.UUID(row.CreatedBy.Bytes)
		principal.ActorID = &actor
	}
	return principal, nil
}

// pathUUID reads an identifier out of the route.
//
// A malformed one answers 404 rather than 400, for the same reason a
// cross-tenant identifier does: the two must be indistinguishable, or the
// difference between them enumerates what exists.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", "no such "+name)
		return uuid.Nil, false
	}
	return id, true
}
