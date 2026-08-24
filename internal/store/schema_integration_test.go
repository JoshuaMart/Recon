//go:build integration

package store_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/store"
)

// exempt lists the tables that carry no org_id, each for a reason. Anything
// else has to have one, so adding a table is a decision rather than an
// omission: the columns are what is urgent, not the policy that will read them.
var exempt = map[string]string{
	"org":                 "it is the tenant",
	"app_user":            "a person can belong to several organizations, which is the whole point of the join table",
	"generic_pivot_value": "reference data shared by every tenant, seeded from the repository",
	"ct_feed_minute":      "one Certificate Transparency socket serves the whole deployment, so when it was alive is a fact about the feed",
	"goose_db_version":    "not a business table",
}

func TestEveryBusinessTableCarriesTheTenant(t *testing.T) {
	url := start(t)
	ctx := context.Background()

	if err := migrator(t, url).Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}
	conn := connect(t, url)

	rows, err := conn.Query(ctx, `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public'
		   AND c.relkind IN ('r', 'p')
		   -- A partition inherits its parent's columns, so checking it would
		   -- be checking the same table twice.
		   AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
		   AND NOT EXISTS (
		       SELECT 1 FROM pg_attribute a
		        WHERE a.attrelid = c.oid AND a.attname = 'org_id' AND a.attnum > 0)
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ok := exempt[name]; !ok {
			missing = append(missing, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these tables carry no org_id and are not on the exempt list: %v\n"+
			"Add the column, or add the table to `exempt` with the reason it does not need one.", missing)
	}
}

// No DEFAULT partition, deliberately: a row whose month is missing must fail
// loudly, because that is the only signal the creation mechanism has broken. A
// default partition would absorb it in silence.
func TestARowOutsideEveryPartitionFailsAndOneInsideDoesNot(t *testing.T) {
	url := start(t)
	ctx := context.Background()

	if err := migrator(t, url).Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}
	conn := connect(t, url)
	asset := seedOneAsset(t, conn)

	_, err := conn.Exec(ctx, `
		INSERT INTO observation (org_id, asset_id, observed_at, last_confirmed_at, source, layer, outcome, data)
		VALUES ($1, $2, '2019-01-01', '2019-01-01', 'test', 'dns', 'ok', '{}')`,
		tenantID, asset)
	if err == nil {
		t.Error("an observation dated outside every partition was accepted, so a broken " +
			"partition job would be invisible until somebody counted rows")
	}

	// The positive control. Without it the refusal above passes just as well on
	// a table nothing can be written to at all.
	if _, err := conn.Exec(ctx, `
		INSERT INTO observation (org_id, asset_id, observed_at, last_confirmed_at, source, layer, outcome, data)
		VALUES ($1, $2, now(), now(), 'test', 'dns', 'ok', '{}')`,
		tenantID, asset); err != nil {
		t.Fatalf("an observation dated now was refused too: %v", err)
	}
}

// Creating a partition is the only DDL the application is allowed, and only
// through that door. The purge stays with the owner: a role that can drop a
// month of observations can lose the one thing this system cannot rebuild.
func TestTheApplicationMayCreateAPartitionAndMayNotPurge(t *testing.T) {
	url := start(t)
	ctx := context.Background()

	if err := migrator(t, url).Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}
	ownerConn := connect(t, url)
	exec(t, ownerConn, fmt.Sprintf(`ALTER ROLE asm_app WITH LOGIN PASSWORD %s`, quote(appPwd)))

	appConn := connect(t, appURL(url))

	// PostgreSQL grants EXECUTE on a new function to PUBLIC, so "not granted"
	// is not a state a function starts in. This pair is what proves the
	// revoke happened rather than the grant being decoration.
	var created int
	if err := appConn.QueryRow(ctx,
		`SELECT ensure_monthly_partitions('observation', 3)`).Scan(&created); err != nil {
		t.Fatalf("the application cannot create a partition, and the scheduler runs as it: %v", err)
	}

	if _, err := appConn.Exec(ctx,
		`SELECT drop_monthly_partitions_before('observation', CURRENT_DATE)`); err == nil {
		t.Error("the application role can call the purge: EXECUTE is granted to PUBLIC by " +
			"default, so it has to be revoked rather than merely not granted")
	}
}

const (
	tenantID  = "11111111-1111-1111-1111-111111111111"
	programID = "22222222-2222-2222-2222-222222222222"
	assetID   = "33333333-3333-3333-3333-333333333333"
)

// seedOneAsset writes the minimum an observation needs to hang off.
func seedOneAsset(t *testing.T, conn *pgx.Conn) string {
	t.Helper()

	exec(t, conn, `INSERT INTO org (id, name) VALUES ($1, 'tenant')`, tenantID)
	exec(t, conn, `
		INSERT INTO program (id, org_id, name, authorized_from)
		VALUES ($1, $2, 'programme', now())`, programID, tenantID)
	exec(t, conn, `
		INSERT INTO asset (id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES ($1, $2, $3, 'fqdn', 'api.target.test', 'api.target.test', 'manual', 'in_scope', now(), now())`,
		assetID, tenantID, programID)

	return assetID
}

// The half of the role assertion that needed the business tables, which is why
// it moved here from the first milestone rather than being ticked on a table
// the test invented for itself.
func TestTheApplicationRoleAgainstTheRealTables(t *testing.T) {
	url := start(t)
	ctx := context.Background()

	if err := migrator(t, url).Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}

	ownerConn := connect(t, url)
	exec(t, ownerConn, fmt.Sprintf(`ALTER ROLE asm_app WITH LOGIN PASSWORD %s`, quote(appPwd)))

	// Reference data comes from the repository, applied by the owner.
	written, err := store.Seed(ctx, ownerConn)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if written == 0 {
		t.Fatal("the seed wrote nothing, so the read below would prove nothing")
	}

	seedOneAsset(t, ownerConn)
	appConn := connect(t, appURL(url))

	const writeAnAsset = `
		INSERT INTO asset (id, org_id, program_id, kind, key, host, discovery_source, scope_status, first_seen, last_seen)
		VALUES (gen_random_uuid(), $1, $2, 'fqdn', 'written-by-the-app.target.test', 'written-by-the-app.target.test',
		        'manual', 'in_scope', now(), now())`

	// The same write twice, and the difference between them is the whole of
	// the isolation: with the organization set it lands, without it the policy
	// refuses. Before row-level security the second one succeeded.
	tx, err := appConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set the organization: %v", err)
	}
	if _, err := tx.Exec(ctx, writeAnAsset, tenantID, programID); err != nil {
		t.Fatalf("the application role cannot write into its own organization: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := appConn.Exec(ctx, writeAnAsset, tenantID, programID); err == nil {
		t.Error("a write with no organization set was accepted, so the policy is not applied")
	}

	var patterns int
	if err := appConn.QueryRow(ctx, `SELECT count(*) FROM generic_pivot_value`).Scan(&patterns); err != nil {
		t.Fatalf("the application cannot read the denylist, and the search path needs it: %v", err)
	}
	if patterns != written {
		t.Errorf("the application reads %d patterns, the seed wrote %d", patterns, written)
	}

	// And what it must not.
	refuses(t, appConn, "DROP TABLE asset", `DROP TABLE asset`)
	refuses(t, appConn, "writing the denylist",
		`INSERT INTO generic_pivot_value (pivot_type, pattern) VALUES ('cookie_name', 'smuggled')`)
	refuses(t, appConn, "deleting from the denylist", `DELETE FROM generic_pivot_value`)
}

// The seed replaces rather than merges: an entry added outside the repository
// must not survive a deployment, or the divergence is invisible.
func TestTheSeedReplacesRatherThanMerges(t *testing.T) {
	url := start(t)
	ctx := context.Background()

	if err := migrator(t, url).Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}
	conn := connect(t, url)

	if _, err := store.Seed(ctx, conn); err != nil {
		t.Fatalf("seed: %v", err)
	}
	exec(t, conn, `INSERT INTO generic_pivot_value (pivot_type, pattern, note)
	               VALUES ('cookie_name', 'added-by-hand', 'not from the repository')`)

	// Replayed, which is what every deployment does.
	if _, err := store.Seed(ctx, conn); err != nil {
		t.Fatalf("reseed: %v", err)
	}

	var survivors int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM generic_pivot_value WHERE pattern = 'added-by-hand'`).Scan(&survivors); err != nil {
		t.Fatalf("read: %v", err)
	}
	if survivors != 0 {
		t.Error("an entry added outside the repository survived a deployment, and nothing " +
			"would ever show the divergence")
	}
}
