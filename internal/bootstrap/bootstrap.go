// Package bootstrap creates the first organization.
//
// Until this exists the only way into the system is to write four rows by hand
// in SQL, which is not a missing convenience: it makes the deployment
// impractical and multi-tenancy untestable, since checking that one tenant
// cannot see another's inventory starts by creating a second tenant, and nobody
// does that without being told.
//
// It runs as the owner, through the migration string, therefore outside the
// application path. That is the decision rather than a detail of packaging. An
// endpoint that creates an organization could not be authenticated, since there
// is no tenant to attach the caller to before one exists, and an unauthenticated
// endpoint that mints a credential is the classic hole in this spot.
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

// ConsoleScopes is what the token a console pastes has to hold.
//
// Everything but ingestion. A console reads the inventory, edits perimeters and
// starts runs; writing a report is a run's action and nothing else's, and a
// credential holding both would let whoever holds the console's key deliver
// observations of their choosing.
var ConsoleScopes = []string{
	string(auth.ActionReadAssets),
	string(auth.ActionManageScope),
	string(auth.ActionManageJobs),
}

// Request is what the operator asked for.
type Request struct {
	// Org names the organization. Used on creation and ignored when the account
	// already exists, because renaming is not this command's job.
	Org string
	// Email identifies the person, and it is what makes the command replayable.
	// app_user.email is UNIQUE in the schema, so the database enforces the
	// identity rather than this code hoping it holds.
	Email string
	// TokenName labels the credential in api_token. Empty takes "bootstrap".
	TokenName string
	// MintToken asks for a credential. On a first run it is implied; on a
	// replay it is the only reason to run the command again, since the secret
	// of the first one was printed and never stored.
	MintToken bool
}

// Result is what happened, so the caller can print the right thing.
//
// A caller that cannot tell a creation from a replay prints "created" either
// way, and somebody goes looking for a tenant that was never made.
type Result struct {
	OrgID       uuid.UUID
	UserID      uuid.UUID
	OrgCreated  bool
	UserCreated bool
	// Token is the secret, and this is the only moment it exists in readable
	// form: only its hash reaches the database. Empty when none was asked for.
	Token string
}

// Refusals rather than defaults. A bootstrap that invents a name creates an
// organization nobody meant to create.
var (
	ErrNoOrg   = errors.New("an organization name is required")
	ErrNoEmail = errors.New("an email address is required")
)

// Run creates the organization, the user, the membership and, if asked, a token.
//
// All of it in one transaction. Four rows that describe one account are either
// all there or none of them are: a half bootstrapped tenant is worse than no
// tenant, because the next run finds the organization and stops before creating
// what is missing.
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

	// The person first, because the person is the anchor.
	//
	// Replayability keys on the email and not on the organization name, and the
	// difference is not cosmetic. app_user.email carries a UNIQUE constraint and
	// org.name deliberately does not, since two customers may legitimately be
	// called the same thing, so matching on the name would be this code hoping
	// where the database enforces nothing.
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

	// The organization this person already belongs to, reached through the
	// membership rather than by name, for the reason above. Ordered on the
	// identifier because membership carries no timestamp: an arbitrary order
	// would make a replay pick a different organization on different runs, and
	// this has to be the same answer every time.
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
		// 'owner' is the only role the model carries today, so it is written
		// rather than parameterized: a flag whose one value is the default is a
		// flag somebody has to be told about.
		if _, err := tx.Exec(ctx,
			`INSERT INTO membership (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
			result.UserID, result.OrgID); err != nil {
			return Result{}, fmt.Errorf("create the membership: %w", err)
		}
		result.OrgCreated = true
		// A first run always hands back a credential. Asking for a flag on the
		// very first bootstrap would be a step nobody can skip.
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

// mintToken writes a token and returns its secret.
//
// The token belongs to the organization and not to the person who asked for it,
// so it survives their departure. created_by records who asked, which is
// attribution and not ownership.
func mintToken(ctx context.Context, tx pgx.Tx, orgID, userID uuid.UUID, name string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	secret := hex.EncodeToString(raw)

	// Hashed through the same function the guard hashes a presented credential
	// with. Two implementations of "how a token becomes a row" is how a token
	// that was minted stops verifying.
	if _, err := tx.Exec(ctx, `
		INSERT INTO api_token (id, org_id, name, token_hash, scopes, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New(), orgID, name, auth.Hash(secret), ConsoleScopes, userID); err != nil {
		return "", fmt.Errorf("create the token: %w", err)
	}
	return secret, nil
}
