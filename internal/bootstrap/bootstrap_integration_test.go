//go:build integration

// The milestone 7 assertion behind `recon bootstrap`: it creates a usable
// organization without a line of SQL, and replayed it creates no second one.
//
// "Usable" is the half worth a container. A command that writes four rows nobody
// can authenticate against has not bootstrapped anything, so the token it prints
// is resolved here through the same statement the guard resolves a request with.
package bootstrap_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/bootstrap"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

func newDatabase(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("recon"),
		tcpostgres.WithUsername("asm_owner"),
		tcpostgres.WithPassword("owner-password-for-a-container"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	migrator, err := store.NewMigrator(url, quiet)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := migrator.Run(ctx, store.Up); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = migrator.Close()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return url, pool
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The first half: four rows, and a credential that authenticates.
func TestABootstrappedOrganizationIsUsable(t *testing.T) {
	url, pool := newDatabase(t)
	ctx := context.Background()

	result, err := bootstrap.Run(ctx, connect(t, url), bootstrap.Request{
		Org: "Tenant", Email: "Person@Example.test",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !result.OrgCreated || !result.UserCreated {
		t.Fatalf("a first run reported org=%v user=%v, and both were created",
			result.OrgCreated, result.UserCreated)
	}
	if result.Token == "" {
		t.Fatal("a first run minted no token, so the account it created cannot be reached")
	}

	if got := count(t, pool, `SELECT count(*) FROM membership WHERE user_id = $1 AND org_id = $2`,
		result.UserID, result.OrgID); got != 1 {
		t.Errorf("membership rows = %d, want 1: an org and a user with nothing joining them "+
			"is an account nobody belongs to", got)
	}
	// Lowercased on the way in. An address that round trips with its original
	// case would make the replay below depend on how somebody typed it.
	var email string
	if err := pool.QueryRow(ctx, `SELECT email FROM app_user WHERE id = $1`, result.UserID).
		Scan(&email); err != nil {
		t.Fatalf("read the user: %v", err)
	}
	if email != "person@example.test" {
		t.Errorf("email = %q, want it folded to lower case", email)
	}

	// The assertion that makes "usable" mean something: the printed secret
	// resolves through the statement the guard uses, and comes back holding the
	// three actions a console needs.
	row, err := sqlcgen.New(pool).PrincipalForToken(ctx, sqlcgen.PrincipalForTokenParams{
		TokenHash: auth.Hash(result.Token),
		At:        pgtimestamp(time.Now()),
	})
	if err != nil {
		t.Fatalf("resolve the printed token: %v", err)
	}
	if uuid.UUID(row.OrgID.Bytes) != result.OrgID {
		t.Errorf("the token resolves to %s, want the organization it was minted for, %s",
			uuid.UUID(row.OrgID.Bytes), result.OrgID)
	}

	principal := auth.Principal{Actions: auth.Actions(row.Scopes)}
	for _, action := range []auth.Action{
		auth.ActionReadAssets, auth.ActionManageScope, auth.ActionManageJobs,
	} {
		if !principal.Can(action) {
			t.Errorf("the bootstrap token does not hold %s, so the console cannot use it", action)
		}
	}
	// And not the run's action. A console credential that could deliver reports
	// would let whoever holds it write observations of its choosing.
	if principal.Can(auth.ActionIngest) {
		t.Error("the bootstrap token holds ingest, which belongs to a run and to nothing else")
	}

	// Only the hash is stored, which is what makes printing it once honest.
	if got := count(t, pool,
		`SELECT count(*) FROM api_token WHERE token_hash = $1::bytea`,
		[]byte(result.Token)); got != 0 {
		t.Error("the token value is in the database, so printing it once claims a property it lacks")
	}
}

// The second half: replayed, it finds rather than creates.
func TestAReplayCreatesNoSecondOrganization(t *testing.T) {
	url, pool := newDatabase(t)
	ctx := context.Background()

	first, err := bootstrap.Run(ctx, connect(t, url), bootstrap.Request{
		Org: "Tenant", Email: "person@example.test",
	})
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// Replayed with a different organization name and a differently cased
	// address, which is the shape a second invocation actually takes. Neither
	// is allowed to produce a second tenant: the email is the key, and renaming
	// is not this command's job.
	second, err := bootstrap.Run(ctx, connect(t, url), bootstrap.Request{
		Org: "A Different Name", Email: "PERSON@example.test",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.OrgCreated || second.UserCreated {
		t.Errorf("a replay reported org=%v user=%v, and it found both",
			second.OrgCreated, second.UserCreated)
	}
	if second.OrgID != first.OrgID || second.UserID != first.UserID {
		t.Errorf("a replay answered org %s user %s, want %s and %s",
			second.OrgID, second.UserID, first.OrgID, first.UserID)
	}
	if got := count(t, pool, `SELECT count(*) FROM org`); got != 1 {
		t.Errorf("org rows = %d, want 1: a bootstrap that duplicates is what produces "+
			"two tenants with one name, one of them empty", got)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM org WHERE id = $1`, first.OrgID).Scan(&name); err != nil {
		t.Fatalf("read the org: %v", err)
	}
	if name != "Tenant" {
		t.Errorf("org name = %q, want the first one: a replay does not rename", name)
	}

	// A replay mints nothing unless asked. The first secret is gone, so this is
	// the only reason to run the command a second time, and it must be a
	// deliberate one.
	if second.Token != "" {
		t.Error("a replay minted a token nobody asked for")
	}
	if got := count(t, pool, `SELECT count(*) FROM api_token`); got != 1 {
		t.Errorf("token rows = %d, want the one the first run minted", got)
	}

	third, err := bootstrap.Run(ctx, connect(t, url), bootstrap.Request{
		Org: "Tenant", Email: "person@example.test", MintToken: true, TokenName: "console",
	})
	if err != nil {
		t.Fatalf("replay with a token: %v", err)
	}
	if third.Token == "" {
		t.Fatal("a replay asked for a token and got none, which is the only reason to run it again")
	}
	if got := count(t, pool, `SELECT count(*) FROM api_token WHERE org_id = $1`, first.OrgID); got != 2 {
		t.Errorf("token rows = %d, want 2 on the same organization", got)
	}
}

// A refusal rather than a default. A bootstrap that invents a name creates an
// organization nobody meant to create, and it is idempotent afterwards, so the
// mistake is permanent.
func TestAMissingNameOrAddressIsRefused(t *testing.T) {
	url, pool := newDatabase(t)
	ctx := context.Background()

	for _, request := range []bootstrap.Request{
		{Email: "person@example.test"},
		{Org: "Tenant"},
		{Org: "   ", Email: "  "},
	} {
		if _, err := bootstrap.Run(ctx, connect(t, url), request); err == nil {
			t.Errorf("bootstrap accepted %+v", request)
		}
	}
	if got := count(t, pool, `SELECT count(*) FROM org`); got != 0 {
		t.Errorf("org rows = %d after three refusals, want none", got)
	}
}

// pgtimestamp is the shape sqlc wants, kept here rather than reached for from
// the api package: a test that imports a handler package to borrow a converter
// is a test that stops compiling when the handler moves.
func pgtimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}
