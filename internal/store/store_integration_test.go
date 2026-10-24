//go:build integration

package store_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/storetest"
)

func startPostgres(t *testing.T) (*postgres.PostgresContainer, string) {
	t.Helper()
	return storetest.Start(t)
}

func TestMigrationsGoUpAndDown(t *testing.T) {
	ctx := context.Background()
	_, url := startPostgres(t)
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p, sqlDB, err := st.Migrator()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := st.Q.InstanceCreatedAt(ctx); err != nil {
		t.Fatalf("baseline row missing after up: %v", err)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to zero: %v", err)
	}
	if _, err := st.Q.InstanceCreatedAt(ctx); err == nil {
		t.Fatal("instance table still exists after migrating down")
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again after down: %v", err)
	}
}

// TestBackupRestores runs the backup script the compose stack runs, against
// the same image, and restores the dump it wrote.
func TestBackupRestores(t *testing.T) {
	ctx := context.Background()
	c, url := startPostgres(t)
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	want, err := st.Q.InstanceCreatedAt(ctx)
	if err != nil {
		t.Fatal(err)
	}

	_, here, _, _ := runtime.Caller(0)
	script, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "compose", "backup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CopyToContainer(ctx, script, "/backup.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PGHOST=localhost", "PGUSER=recon", "PGPASSWORD=" + storetest.Password, "PGDATABASE=recon", "BACKUP_DIR=/backups"}

	file := strings.TrimSpace(run(t, c, env, "/backup.sh", "once"))
	if !strings.HasPrefix(file, "/backups/recon-") || !strings.HasSuffix(file, ".dump") {
		t.Fatalf("unexpected dump path %q", file)
	}
	out := run(t, c, env, "/backup.sh", "verify", file)
	if !strings.Contains(out, "restore ok") {
		t.Fatalf("verify output: %s", out)
	}

	// A real restore over the live database, after the data was damaged.
	if _, err := st.Pool.Exec(ctx, "UPDATE instance SET created_at = now() + interval '1 day'"); err != nil {
		t.Fatal(err)
	}
	run(t, c, env, "/backup.sh", "restore", file)
	got, err := st.Q.InstanceCreatedAt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("created_at after restore = %v, want %v", got, want)
	}
}

func TestVerifyRejectsATruncatedDump(t *testing.T) {
	ctx := context.Background()
	c, url := startPostgres(t)
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.CopyToContainer(ctx, []byte("PGDMP not a dump"), "/backups/broken.dump", 0o644); err != nil {
		t.Fatal(err)
	}
	_, here, _, _ := runtime.Caller(0)
	script, _ := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "compose", "backup.sh"))
	if err := c.CopyToContainer(ctx, script, "/backup.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PGHOST=localhost", "PGUSER=recon", "PGPASSWORD=" + storetest.Password, "PGDATABASE=recon"}
	code, _ := exitCode(t, c, env, "/backup.sh", "verify", "/backups/broken.dump")
	if code == 0 {
		t.Fatal("verify accepted a file that is not a dump")
	}
}

func run(t *testing.T, c testcontainers.Container, env []string, cmd ...string) string {
	t.Helper()
	code, out := exitCode(t, c, env, cmd...)
	if code != 0 {
		t.Fatalf("%v exited %d: %s", cmd, code, out)
	}
	return out
}

func exitCode(t *testing.T, c testcontainers.Container, env []string, cmd ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code, r, err := c.Exec(ctx, cmd, exec.Multiplexed(), exec.WithEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return code, buf.String()
}

// A restore from a dump cut short must fail loudly and leave the database as
// it was: a pipeline without pipefail would report it as done.
func TestRestoreOfATruncatedDumpFailsAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	c, url := startPostgres(t)
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	_, here, _, _ := runtime.Caller(0)
	script, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "compose", "backup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CopyToContainer(ctx, script, "/backup.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PGHOST=localhost", "PGUSER=recon", "PGPASSWORD=" + storetest.Password, "PGDATABASE=recon", "BACKUP_DIR=/backups"}
	file := strings.TrimSpace(run(t, c, env, "/backup.sh", "once"))
	run(t, c, env, "sh", "-c", `size=$(wc -c < `+file+`); head -c $((size * 3 / 4)) `+file+` > /backups/cut.dump`)

	marker := "after the dump"
	if _, err := st.Pool.Exec(ctx, "CREATE TABLE marker (v text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, "INSERT INTO marker VALUES ($1)", marker); err != nil {
		t.Fatal(err)
	}
	if code, out := exitCode(t, c, env, "/backup.sh", "restore", "/backups/cut.dump"); code == 0 {
		t.Fatalf("restoring a truncated dump exited 0: %s", out)
	}
	var got string
	if err := st.Pool.QueryRow(ctx, "SELECT v FROM marker").Scan(&got); err != nil || got != marker {
		t.Fatalf("a failed restore changed the database: %q, %v", got, err)
	}
}
