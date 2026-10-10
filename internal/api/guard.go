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
// Inline checks would force an audit of every endpoint when a second role appears.
type Guard struct {
	// system is the pool crossing tenants, as a token names itself and never an organization:
	// this query discovers the tenant and cannot be filtered by one. Subjecting it to the
	// policies would force a predicate nobody can satisfy or a table quietly exempted, and the
	// second gets chosen under pressure. One statement keyed by a hash the caller must hold;
	// everything after runs scoped.
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
			// One answer for a missing, wrong or expired credential; which belongs in a log.
			g.log.WarnContext(r.Context(), "request refused", "path", r.URL.Path, "reason", err)
			fail(w, http.StatusUnauthorized, "unauthorized", "the credential is missing, wrong or expired")
			return
		}
		if !principal.Can(action) {
			// Distinct on purpose: an authenticated caller lacking a privilege has a different
			// problem from a wrong credential, and the distinction makes the separation of
			// actions usable.
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

// pathUUID reads an identifier out of the route. A malformed one answers 404, not
// 400, like a cross-tenant one: they must be indistinguishable or the difference
// enumerates what exists.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", "no such "+name)
		return uuid.Nil, false
	}
	return id, true
}
