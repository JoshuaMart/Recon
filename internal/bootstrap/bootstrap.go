// Package bootstrap creates the first organization. Without it the only way in is writing
// four rows by hand in SQL, which makes multi-tenancy untestable (checking tenant isolation
// starts with creating a second tenant).
//
// It runs as the owner, through the migration string, outside the application path: an
// endpoint creating an organization could not be authenticated (no tenant exists to attach
// the caller to), and an unauthenticated endpoint minting a credential is the classic hole.
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/auth"
)

// ConsoleScopes is what the token a console pastes has to hold: everything but
// ingestion. Writing a report is a run's action; a credential holding both would
// let the console's key holder deliver observations of their choosing.
var ConsoleScopes = []string{
	string(auth.ActionReadAssets),
	string(auth.ActionManageScope),
	string(auth.ActionManageJobs),
}

// Request is what the operator asked for.
type Request struct {
	// Org names the organization. Used on creation, ignored when the account exists
	// (renaming is not this command's job).
	Org string
	// Email identifies the person and makes the command replayable: app_user.email is
	// UNIQUE, so the database enforces the identity.
	Email string
	// TokenName labels the credential in api_token. Empty takes "bootstrap".
	TokenName string
	// MintToken asks for a credential. Implied on a first run; on a replay it is the
	// only reason to run again, as the first secret was printed and never stored.
	MintToken bool
}

// Result is what happened. A caller unable to tell a creation from a replay prints
// "created" either way, sending someone looking for a tenant never made.
type Result struct {
	OrgID       uuid.UUID
	UserID      uuid.UUID
	OrgCreated  bool
	UserCreated bool
	// Token is the secret, readable only at this moment (only its hash reaches the
	// database). Empty when none was asked for.
	Token string
}

// Refusals, not defaults: a bootstrap inventing a name creates an organization
// nobody meant.
var (
	ErrNoOrg   = errors.New("an organization name is required")
	ErrNoEmail = errors.New("an email address is required")
)

// Run creates the organization, user, membership and, if asked, a token, in one
// transaction: a half-bootstrapped tenant is worse than none, since the next run
// finds the organization and stops before creating what is missing.
func Run(ctx context.Context, conn *pgx.Conn, request Request) (Result, error) {
	org := strings.TrimSpace(request.Org)
	email := strings.ToLower(strings.TrimSpace(request.Email))
	if org == "" {
		return Result{}, ErrNoOrg
	}
	if email == "" {
		return Result{}, ErrNoEmail
	}
	name := strings.TrimSpace(request.TokenName)
	if name == "" {
		name = "bootstrap"
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := create(ctx, tx, org, email, name, request.MintToken)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

func create(ctx context.Context, tx pgx.Tx, org, email, tokenName string, mint bool) (Result, error) {
	var result Result

	// The person first, as the anchor. Replayability keys on the email, not the
	// organization name: app_user.email is UNIQUE while org.name deliberately is not
	// (two customers may share a name), so matching on name would hope where the
	// database enforces nothing.
	err := tx.QueryRow(ctx, `SELECT id FROM app_user WHERE email = $1`, email).Scan(&result.UserID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		result.UserID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO app_user (id, email) VALUES ($1, $2)`, result.UserID, email); err != nil {
			return Result{}, fmt.Errorf("create the user: %w", err)
		}
		result.UserCreated = true
	case err != nil:
		return Result{}, fmt.Errorf("look up the user: %w", err)
	}

	// The organization this person already belongs to, via the membership, not by
	// name. Ordered by identifier because membership has no timestamp: an arbitrary
	// order would pick a different organization on different replays.
	err = tx.QueryRow(ctx,
		`SELECT org_id FROM membership WHERE user_id = $1 ORDER BY org_id LIMIT 1`,
		result.UserID).Scan(&result.OrgID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		result.OrgID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO org (id, name) VALUES ($1, $2)`, result.OrgID, org); err != nil {
			return Result{}, fmt.Errorf("create the organization: %w", err)
		}
		// 'owner' is the model's only role today, so it is written, not parameterized.
		if _, err := tx.Exec(ctx,
			`INSERT INTO membership (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
			result.UserID, result.OrgID); err != nil {
			return Result{}, fmt.Errorf("create the membership: %w", err)
		}
		result.OrgCreated = true
		// A first run always hands back a credential; requiring a flag there is a step
		// nobody can skip.
		mint = true
	case err != nil:
		return Result{}, fmt.Errorf("look up the membership: %w", err)
	}

	if mint {
		token, err := mintToken(ctx, tx, result.OrgID, result.UserID, tokenName)
		if err != nil {
			return Result{}, err
		}
		result.Token = token
	}
	return result, nil
}

// mintToken writes a token and returns its secret. The token belongs to the
// organization, not the requester, so it survives their departure; created_by is
// attribution, not ownership.
func mintToken(ctx context.Context, tx pgx.Tx, orgID, userID uuid.UUID, name string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	secret := hex.EncodeToString(raw)

	// Hashed with the function the guard hashes a presented credential with; two
	// implementations of "how a token becomes a row" make a minted token stop verifying.
	if _, err := tx.Exec(ctx, `
		INSERT INTO api_token (id, org_id, name, token_hash, scopes, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New(), orgID, name, auth.Hash(secret), ConsoleScopes, userID); err != nil {
		return "", fmt.Errorf("create the token: %w", err)
	}
	return secret, nil
}
