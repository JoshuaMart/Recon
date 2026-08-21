//go:build integration

// Milestone 0, in the two assertions a database can answer: a migration can be
// applied then rolled back without loss, and the application role is confined
// to what the default privileges give it.
//
// Behind a build tag, because it needs a container. `make test` runs the unit
// tests; `make test-integration` runs these.
package store_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JoshuaMart/recon/internal/store"
)

const (
	owner    = "asm_owner"
	ownerPwd = "owner-password-for-a-container"
	appPwd   = "app-password-for-a-container"
	database = "recon"
)

// start brings up a PostgreSQL matching the one the stack runs, and returns the
// owner's connection string.
func start(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase(database),
		tcpostgres.WithUsername(owner),
		tcpostgres.WithPassword(ownerPwd),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return url
}

func migrator(t *testing.T, url string) *store.Migrator {
	t.Helper()

	m, err := store.NewMigrator(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func exec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()

	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// refuses runs a statement expecting it to fail, and says which one did not.
func refuses(t *testing.T, conn *pgx.Conn, what, sql string) {
	t.Helper()

	if _, err := conn.Exec(context.Background(), sql); err == nil {
		t.Errorf("%s succeeded, and the application role is meant to be refused it", what)
	}
}

func roleExists(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()

	var exists bool
	err := conn.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1)`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("read pg_roles: %v", err)
	}
	return exists
}

func TestASchemaCanBeUnwoundAndReplayedWithoutLoss(t *testing.T) {
	url := start(t)
	ctx := context.Background()
	m := migrator(t, url)

	if err := m.Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}
	conn := connect(t, url)
	for _, role := range []string{"asm_app", "asm_sys"} {
		if !roleExists(t, conn, role) {
			t.Fatalf("%s was not created by the migration", role)
		}
	}

	// Data the migration does not own. Rolling the schema back must not touch
	// it, which is what "without loss" means here.
	exec(t, conn, `CREATE TABLE probe (id int PRIMARY KEY, note text)`)
	exec(t, conn, `INSERT INTO probe VALUES (1, 'written before the rollback')`)

	if err := m.Run(ctx, store.Reset); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, role := range []string{"asm_app", "asm_sys"} {
		if roleExists(t, conn, role) {
			t.Errorf("%s survived the rollback, so the migration is not reversible", role)
		}
	}

	var note string
	if err := conn.QueryRow(ctx, `SELECT note FROM probe WHERE id = 1`).Scan(&note); err != nil {
		t.Fatalf("the data did not survive the rollback: %v", err)
	}
	if note != "written before the rollback" {
		t.Errorf("note = %q, want the row written before the rollback", note)
	}

	if err := m.Run(ctx, store.Up); err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, role := range []string{"asm_app", "asm_sys"} {
		if !roleExists(t, conn, role) {
			t.Errorf("%s did not come back on replay", role)
		}
	}

	version, err := m.Version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if version == 0 {
		t.Error("the schema reports version 0 after a replay")
	}
}

func TestTheApplicationRoleIsConfinedToItsPrivileges(t *testing.T) {
	url := start(t)
	ctx := context.Background()

	if err := migrator(t, url).Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}

	ownerConn := connect(t, url)

	// The migration leaves the roles NOLOGIN and without a password: granting
	// those is the deployment's job. Here the test plays the deployment.
	exec(t, ownerConn, fmt.Sprintf(`ALTER ROLE asm_app WITH LOGIN PASSWORD %s`, quote(appPwd)))

	// Created after the ALTER DEFAULT PRIVILEGES, which is what those cover:
	// a table that predates them would need a manual grant, and this test
	// would then be measuring the grant rather than the defaults.
	exec(t, ownerConn, `CREATE TABLE inventory (id int PRIMARY KEY, note text)`)

	appConn := connect(t, appURL(url))

	// What the application must be able to do, through the default privileges
	// alone. This half first: without it every refusal below would pass just as
	// well on a role that cannot do anything at all.
	exec(t, appConn, `INSERT INTO inventory VALUES (1, 'written by the application role')`)
	var note string
	if err := appConn.QueryRow(ctx, `SELECT note FROM inventory WHERE id = 1`).Scan(&note); err != nil {
		t.Fatalf("the application role cannot read a table the owner created: %v", err)
	}
	exec(t, appConn, `UPDATE inventory SET note = 'updated' WHERE id = 1`)
	exec(t, appConn, `DELETE FROM inventory WHERE id = 1`)

	// And what it must not.
	refuses(t, appConn, "CREATE TABLE", `CREATE TABLE forbidden (id int)`)
	refuses(t, appConn, "DROP TABLE", `DROP TABLE inventory`)
	refuses(t, appConn, "CREATE ROLE", `CREATE ROLE intruder NOLOGIN`)
	refuses(t, appConn, "ALTER TABLE", `ALTER TABLE inventory ADD COLUMN extra text`)
}

// quote renders a literal for a statement that cannot take a parameter. ALTER
// ROLE is one: PostgreSQL does not accept a bind parameter for a password.
func quote(s string) string {
	return "'" + s + "'"
}

func appURL(ownerURL string) string {
	cfg, err := pgx.ParseConfig(ownerURL)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("postgres://asm_app:%s@%s:%d/%s?sslmode=disable",
		appPwd, cfg.Host, cfg.Port, cfg.Database)
}
