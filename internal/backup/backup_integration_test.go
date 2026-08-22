//go:build integration

// Milestone 0: a backup is restored into an empty database from the configured
// archive destination, and the content is identical.
//
// The archive destination is configuration, a path locally and a bucket in
// production, which is what lets this run on a laptop instead of waiting for a
// production host to exist. An assertion that cannot go green until the end of
// a project is one everyone learns to skip.
//
// The discriminating case is the second batch of rows. They are written *after*
// the base backup, so they exist only in the archived WAL: a restore that
// replayed nothing would still return the first batch and look correct.
package backup_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	owner    = "asm_owner"
	password = "owner-password-for-a-container"
	database = "recon"
	image    = "postgres:18-alpine"

	// The archive lives on a named volume rather than a host directory. Both
	// are "a path" as far as PostgreSQL is concerned, and a volume keeps the
	// test off the ownership mapping a bind mount gets on some runtimes.
	archive = "/archive"
)

func TestARestoreFromTheArchiveReturnsWhatWasWrittenAfterTheBaseBackup(t *testing.T) {
	ctx := context.Background()
	volume := fmt.Sprintf("recon-archive-test-%d", time.Now().UnixNano())

	primary := startPrimary(t, ctx, volume)

	// The archive directory has to exist and be writable by postgres before the
	// first archive_command fires, or every segment fails to ship and the only
	// sign is a line in the log. A fresh volume mounts owned by root, so this
	// step is the deployment's, not the database's.
	runAs(t, ctx, primary, "root", []string{
		"sh", "-c",
		"mkdir -p " + archive + "/wal && chown -R postgres " + archive,
	})

	conn := connect(t, ctx, primary)
	mustExec(t, ctx, conn, `CREATE TABLE inventory (id int PRIMARY KEY, note text)`)
	mustExec(t, ctx, conn, `INSERT INTO inventory VALUES (1, 'before the base backup')`)

	// Force the segment holding those rows into the archive.
	mustExec(t, ctx, conn, `SELECT pg_switch_wal()`)

	// The base backup. -Xs carries the WAL the backup needs to be consistent,
	// so what the archive has to supply is strictly what came afterwards.
	runAs(t, ctx, primary, "postgres", []string{
		"pg_basebackup", "-h", "127.0.0.1", "-U", owner,
		"-D", archive + "/base", "-Fp", "-Xs",
	})

	// Written after the base. These rows exist nowhere but in the archived WAL,
	// which is what makes the assertion below about continuous archiving rather
	// than about copying a directory.
	mustExec(t, ctx, conn, `INSERT INTO inventory VALUES (2, 'after the base backup')`)
	mustExec(t, ctx, conn, `SELECT pg_switch_wal()`)
	mustExec(t, ctx, conn, `CHECKPOINT`)
	waitForArchive(t, ctx, primary, 2)

	// What turns a data directory into one that replays the archive on start.
	runAs(t, ctx, primary, "postgres", []string{"touch", archive + "/base/recovery.signal"})

	restored := startRestored(t, ctx, volume)
	restoredConn := connect(t, ctx, restored)

	var count int
	if err := restoredConn.QueryRow(ctx, `SELECT count(*) FROM inventory`).Scan(&count); err != nil {
		t.Fatalf("read the restored inventory: %v", err)
	}
	if count != 2 {
		t.Fatalf("the restored database holds %d rows, want 2: a restore that replayed "+
			"no WAL would return 1, which is what this count is here to separate", count)
	}

	var note string
	if err := restoredConn.QueryRow(ctx,
		`SELECT note FROM inventory WHERE id = 2`).Scan(&note); err != nil {
		t.Fatalf("the row written after the base backup did not survive: %v", err)
	}
	if note != "after the base backup" {
		t.Errorf("note = %q, want the row written after the base backup", note)
	}
}

func startPrimary(t *testing.T, ctx context.Context, volume string) testcontainers.Container {
	t.Helper()

	return start(t, ctx, testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     owner,
			"POSTGRES_PASSWORD": password,
			"POSTGRES_DB":       database,
		},
		Cmd: []string{
			"postgres",
			"-c", "archive_mode=on",
			// The guard matters: without it a retried archive_command would
			// overwrite a segment that has already shipped.
			"-c", fmt.Sprintf("archive_command=test ! -f %s/wal/%%f && cp %%p %s/wal/%%f", archive, archive),
			"-c", "wal_level=replica",
		},
		HostConfigModifier: func(c *container.HostConfig) {
			c.Binds = append(c.Binds, volume+":"+archive)
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60 * time.Second),
	})
}

func startRestored(t *testing.T, ctx context.Context, volume string) testcontainers.Container {
	t.Helper()

	return start(t, ctx, testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			// A populated data directory makes the entrypoint skip
			// initialization and simply start on what is there.
			"PGDATA":            archive + "/base",
			"POSTGRES_PASSWORD": password,
		},
		Cmd: []string{
			"postgres",
			"-c", fmt.Sprintf("restore_command=cp %s/wal/%%f %%p", archive),
			// The restored directory must not start shipping segments into the
			// archive it was restored from.
			"-c", "archive_mode=off",
		},
		HostConfigModifier: func(c *container.HostConfig) {
			c.Binds = append(c.Binds, volume+":"+archive)
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithStartupTimeout(90 * time.Second),
	})
}

func start(t *testing.T, ctx context.Context, req testcontainers.ContainerRequest) testcontainers.Container {
	t.Helper()

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start container: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	return c
}

func connect(t *testing.T, ctx context.Context, c testcontainers.Container) *pgx.Conn {
	t.Helper()

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	url := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		owner, password, host, port.Port(), database)

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func mustExec(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) {
	t.Helper()

	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func runAs(t *testing.T, ctx context.Context, c testcontainers.Container, user string, cmd []string) {
	t.Helper()

	code, reader, err := c.Exec(ctx, cmd, tcexec.WithUser(user), tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("exec %v: %v", cmd, err)
	}
	if code != 0 {
		out := new(strings.Builder)
		if reader != nil {
			_, _ = fmt.Fprint(out, readAll(reader))
		}
		t.Fatalf("exec %v exited %d: %s", cmd, code, out.String())
	}
}

// waitForArchive blocks until the expected number of segments has shipped.
// Archiving is asynchronous, so asserting on the restore without this would be
// a race that fails on a slow machine and passes on a fast one.
func waitForArchive(t *testing.T, ctx context.Context, c testcontainers.Container, want int) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, reader, err := c.Exec(ctx, []string{
			"sh", "-c",
			"ls " + archive + "/wal | wc -l",
		}, tcexec.Multiplexed())
		if err == nil && code == 0 {
			if n := parseCount(readAll(reader)); n >= want {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("fewer than %d segments reached the archive within the deadline", want)
}
