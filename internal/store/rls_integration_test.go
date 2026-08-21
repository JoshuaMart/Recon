//go:build integration

// Milestone 6's isolation half, and the rule that decides how it is written:
// an isolation property is demonstrated by making it fail, never by observing
// that it holds. Every assertion here either revokes something, unsets
// something, or asks for a tenant it is not.
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/store"
)

const sysPwd = "sys-password-for-a-container"

// tenants is what every case here is measured against: two organizations, each
// with a programme and an asset, and nothing in any query naming either.
type tenants struct {
	one, two           uuid.UUID
	programOne         uuid.UUID
	keyOne, keyTwo     string
	ownerConn          *pgx.Conn
	appConn, sysConn   *pgx.Conn
	appURL, systemURL_ string
}

func setupTenants(t *testing.T) *tenants {
	t.Helper()

	url := start(t)
	ctx := context.Background()
	if err := migrator(t, url).Run(ctx, store.Up); err != nil {
		t.Fatalf("up: %v", err)
	}

	owner := connect(t, url)
	// The migration leaves both roles NOLOGIN and without a password, because
	// granting those is the deployment's job. Here the test plays it.
	exec(t, owner, fmt.Sprintf(`ALTER ROLE asm_app WITH LOGIN PASSWORD %s`, quote(appPwd)))
	exec(t, owner, fmt.Sprintf(`ALTER ROLE asm_sys WITH LOGIN PASSWORD %s`, quote(sysPwd)))

	tn := &tenants{
		one: uuid.New(), two: uuid.New(), programOne: uuid.New(),
		keyOne: "a.one.test", keyTwo: "a.two.test", ownerConn: owner,
	}
	tn.appURL = roleURL(url, "asm_app", appPwd)
	tn.systemURL_ = roleURL(url, "asm_sys", sysPwd)

	programTwo := uuid.New()
	exec(t, owner, `INSERT INTO org (id, name) VALUES ($1, 'one'), ($2, 'two')`, tn.one, tn.two)
	exec(t, owner, `INSERT INTO program (id, org_id, name, authorized_from)
	                VALUES ($1, $2, 'p1', now()), ($3, $4, 'p2', now())`,
		tn.programOne, tn.one, programTwo, tn.two)
	for _, row := range []struct {
		org, program uuid.UUID
		key          string
	}{{tn.one, tn.programOne, tn.keyOne}, {tn.two, programTwo, tn.keyTwo}} {
		id := uuid.New()
		exec(t, owner, `INSERT INTO asset
		    (id, org_id, program_id, kind, key, discovery_source, scope_status, first_seen, last_seen)
		    VALUES ($1, $2, $3, 'fqdn', $4, 'manual', 'in_scope', now(), now())`,
			id, row.org, row.program, row.key)
		exec(t, owner, `INSERT INTO asset_current
		    (asset_id, org_id, program_id, kind, key, scope_status, first_seen, last_seen)
		    VALUES ($1, $2, $3, 'fqdn', $4, 'in_scope', now(), now())`,
			id, row.org, row.program, row.key)
		exec(t, owner, `INSERT INTO observation
		    (org_id, asset_id, observed_at, last_confirmed_at, source, layer, outcome, data)
		    VALUES ($1, $2, now(), now(), 'test', 'dns', 'ok', '{}')`, row.org, id)
	}

	tn.appConn = connect(t, tn.appURL)
	tn.sysConn = connect(t, tn.systemURL_)
	return tn
}

func roleURL(ownerURL, role, password string) string {
	cfg, err := pgx.ParseConfig(ownerURL)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		role, password, cfg.Host, cfg.Port, cfg.Database)
}

// refuseToRunPrivileged is the preamble the doc asks for, and it is not a
// formality.
//
// A policy does not apply to a table's owner, so a suite run as asm_owner
// passes entirely without exercising anything. A modified connection string
// would then make the whole of this file inoperative while leaving the
// milestone green, and a green milestone on an absent property is what teaches
// people to stop reading milestones.
func refuseToRunPrivileged(t *testing.T, conn *pgx.Conn) {
	t.Helper()

	if err := privileged(conn); err != nil {
		t.Fatal(err)
	}
}

// privileged is the check itself, separated from the failure so that the check
// can be tested.
//
// A preamble nobody exercises is a preamble that can stop checking, and this one
// is the thing standing between a modified connection string and a whole file of
// green assertions measuring nothing.
func privileged(conn *pgx.Conn) error {
	ctx := context.Background()
	var role string
	var super, bypass bool
	err := conn.QueryRow(ctx,
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&role, &super, &bypass)
	if err != nil {
		return fmt.Errorf("read the connection's identity: %w", err)
	}
	if super {
		return fmt.Errorf("this suite is connected as %q, which is a superuser: it would pass "+
			"without exercising a single policy", role)
	}
	if bypass {
		return fmt.Errorf("this suite is connected as %q, which carries BYPASSRLS: it would pass "+
			"without exercising a single policy", role)
	}

	var owned []string
	rows, err := conn.Query(ctx, `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
		   AND pg_get_userbyid(c.relowner) = current_user`)
	if err != nil {
		return fmt.Errorf("read ownership: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		owned = append(owned, name)
	}
	rows.Close()
	if len(owned) > 0 {
		return fmt.Errorf("this suite is connected as %q, which owns %s: a policy does not apply "+
			"to a table's owner", role, strings.Join(owned, ", "))
	}
	return nil
}

// TestTheSuiteRefusesToRunPrivileged is the preamble tested rather than
// trusted.
//
// A policy does not apply to a table's owner, so this whole file run as
// asm_owner passes entirely without exercising anything, and a green milestone
// on an absent property is what teaches people to stop reading milestones.
func TestTheSuiteRefusesToRunPrivileged(t *testing.T) {
	tn := setupTenants(t)

	// The owner: a superuser in this container, and the owner of every table.
	if err := privileged(tn.ownerConn); err == nil {
		t.Error("the preamble accepted the owner's connection, so a modified connection string " +
			"would make this file inoperative in silence")
	}

	// The system role: not a superuser, owns nothing, and carries BYPASSRLS,
	// which is the case a check on ownership alone would let through.
	if err := privileged(tn.sysConn); err == nil {
		t.Error("the preamble accepted a connection carrying BYPASSRLS")
	}

	// And the one it is supposed to accept, or the two refusals above measure
	// a check that refuses everything.
	if err := privileged(tn.appConn); err != nil {
		t.Errorf("the preamble refused the application role: %v", err)
	}
}

// asOrg runs one statement in a transaction that carries an organization.
//
// The variable is set at the start of the transaction carrying the query and
// never when the connection is acquired. A plain SET at acquisition survives
// the connection going back to the pool, and the next query from another tenant
// inherits the previous context, which is the leak this mechanism exists to
// prevent introduced by the mechanism meant to prevent it.
func asOrg(t *testing.T, conn *pgx.Conn, org uuid.UUID, query string, args ...any) int {
	t.Helper()

	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if org != uuid.Nil {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, org.String()); err != nil {
			t.Fatalf("set the organization: %v", err)
		}
	}
	var count int
	if err := tx.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return count
}

func TestAnApplicationSessionReadsOneTenantAndNoOther(t *testing.T) {
	tn := setupTenants(t)
	refuseToRunPrivileged(t, tn.appConn)

	// Not one of these statements names org_id. That is the point: the filter
	// the caller can express is the filter the caller can forget.
	for _, table := range []string{"asset", "asset_current", "observation", "program"} {
		if n := asOrg(t, tn.appConn, tn.one, `SELECT count(*) FROM `+table); n != 1 { //nolint:gosec // a literal from this list
			t.Errorf("%s: an organization set to one tenant sees %d rows, want its own 1", table, n)
		}
	}

	var key string
	ctx := context.Background()
	tx, err := tn.appConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, tn.one.String()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT key FROM asset`).Scan(&key); err != nil {
		t.Fatalf("read: %v", err)
	}
	if key != tn.keyOne {
		t.Errorf("key = %q, want %q", key, tn.keyOne)
	}

	// The other tenant's asset, asked for by its own identifier. A policy that
	// only filtered a bare listing would let this one through.
	var found int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM asset WHERE key = $1`, tn.keyTwo).Scan(&found); err != nil {
		t.Fatalf("read: %v", err)
	}
	if found != 0 {
		t.Errorf("an organization reached another's asset by naming it: %d rows", found)
	}
	_ = tx.Commit(ctx)
}

// TestAConnectionWithNoOrganizationReadsNothingTwiceOver is the one that has to
// reuse a connection.
//
// SET LOCAL restores whatever the session held before the transaction, and a
// custom variable never set at session level holds ” rather than nothing. So
// current_setting(...)::uuid returns zero rows on a connection's first
// transaction and raises on its second, and a suite acquiring a fresh
// connection per case passes with that fault in place.
func TestAConnectionWithNoOrganizationReadsNothingTwiceOver(t *testing.T) {
	tn := setupTenants(t)
	refuseToRunPrivileged(t, tn.appConn)

	if n := asOrg(t, tn.appConn, uuid.Nil, `SELECT count(*) FROM asset`); n != 0 {
		t.Fatalf("a connection with no organization set read %d rows, want 0", n)
	}

	// One transaction that does set it, on the same connection, which is what
	// leaves the empty string behind.
	if n := asOrg(t, tn.appConn, tn.one, `SELECT count(*) FROM asset`); n != 1 {
		t.Fatalf("scoped read returned %d, want 1", n)
	}

	if n := asOrg(t, tn.appConn, uuid.Nil, `SELECT count(*) FROM asset`); n != 0 {
		t.Errorf("after a scoped transaction on the same connection, an unscoped one read %d rows, want 0", n)
	}
}

func TestAnApplicationSessionCannotWriteIntoAnotherTenant(t *testing.T) {
	tn := setupTenants(t)
	refuseToRunPrivileged(t, tn.appConn)

	ctx := context.Background()
	tx, err := tn.appConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, tn.one.String()); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Its own tenant first. Without this the refusal below would pass just as
	// well on a role that cannot write at all, and a failed statement aborts
	// the transaction, so the order is not a preference.
	if _, err := tx.Exec(ctx, `INSERT INTO pivot_count (org_id, pivot_type, pivot_value, count)
	                           VALUES ($1, 'favicon', 'its own', 1)`, tn.one); err != nil {
		t.Fatalf("an organization could not write its own row: %v", err)
	}

	// A write is the half a USING clause does not cover. Without WITH CHECK a
	// session could file rows under any tenant it liked and read none of them
	// back, which is worse than reading them.
	if _, err := tx.Exec(ctx, `INSERT INTO pivot_count (org_id, pivot_type, pivot_value, count)
	                           VALUES ($1, 'favicon', 'smuggled', 1)`, tn.two); err == nil {
		t.Error("an organization wrote a row belonging to another")
	}
}

// TestTheCrossingRoleCrossesAndTheFallbackCarriesIt exercises both paths.
//
// A fallback nobody exercises is no better than an absent one: it only adds the
// certainty of having one. So the attribute is taken away in the middle of this
// test and the same assertion is made again.
func TestTheCrossingRoleCrossesAndTheFallbackCarriesIt(t *testing.T) {
	tn := setupTenants(t)

	var bypass bool
	if err := tn.sysConn.QueryRow(context.Background(),
		`SELECT rolbypassrls FROM pg_roles WHERE rolname = 'asm_sys'`).Scan(&bypass); err != nil {
		t.Fatalf("read the attribute: %v", err)
	}
	if !bypass {
		t.Fatal("the migration did not grant BYPASSRLS on a cluster whose owner is a superuser, " +
			"so the main path is not being exercised at all")
	}

	crossings := []string{
		`SELECT count(*) FROM asset`,
		`SELECT count(*) FROM asset_current WHERE lifecycle <> 'archived'`,
		`SELECT count(*) FROM observation`,
	}
	for _, query := range crossings {
		if n := asOrg(t, tn.sysConn, uuid.Nil, query); n != 2 {
			t.Errorf("with BYPASSRLS, %q returned %d, want both tenants", query, n)
		}
	}

	// The managed-database case, produced rather than imagined.
	exec(t, tn.ownerConn, `ALTER ROLE asm_sys NOBYPASSRLS`)
	fallback := connect(t, tn.systemURL_)
	for _, query := range crossings {
		if n := asOrg(t, fallback, uuid.Nil, query); n != 2 {
			t.Errorf("without BYPASSRLS, %q returned %d, want both tenants: the USING (true) "+
				"policy is not carrying the crossing", query, n)
		}
	}

	// And the application role is unaffected by any of that, which is what
	// says the two paths are separate rather than one switch.
	refuseToRunPrivileged(t, tn.appConn)
	if n := asOrg(t, tn.appConn, tn.one, `SELECT count(*) FROM asset`); n != 1 {
		t.Errorf("the application role read %d rows while the system role was being changed, want 1", n)
	}
}

// TestEveryTenantTableCarriesItsPolicies walks the catalog rather than a list.
//
// The fallback moves the property from the role to the policy, therefore onto
// something a migration can forget to carry onto a new table. This is what
// notices, and it counts partitions: a policy on a partitioned parent is
// applied to a partition reached through the parent and to nothing else.
func TestEveryTenantTableCarriesItsPolicies(t *testing.T) {
	tn := setupTenants(t)
	ctx := context.Background()

	rows, err := tn.ownerConn.Query(ctx, `
		SELECT c.relname, c.relrowsecurity,
		       count(*) FILTER (WHERE p.polname = 'tenant_isolation') AS tenant,
		       count(*) FILTER (WHERE p.polname = 'system_crosses')   AS system
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  LEFT JOIN pg_policy p ON p.polrelid = c.oid
		 WHERE n.nspname = 'public'
		   AND c.relkind IN ('r', 'p')
		   AND (c.relname IN ('org', 'app_user')
		        OR EXISTS (SELECT 1 FROM pg_attribute a
		                    WHERE a.attrelid = c.oid AND a.attname = 'org_id'
		                      AND a.attnum > 0 AND NOT a.attisdropped))
		 GROUP BY c.relname, c.relrowsecurity
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var name string
		var enabled bool
		var tenant, system int
		if err := rows.Scan(&name, &enabled, &tenant, &system); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if !enabled {
			t.Errorf("%s carries the tenant and has row-level security disabled", name)
		}
		if tenant != 1 {
			t.Errorf("%s has %d tenant_isolation policies, want 1", name, tenant)
		}
		if system != 1 {
			t.Errorf("%s has %d system_crosses policies, want 1: the fallback would not carry it",
				name, system)
		}
	}
	if seen < 16 {
		t.Errorf("only %d tenant tables were examined, which means the catalog query is wrong "+
			"rather than the schema small", seen)
	}
}

// TestAPartitionCreatedLaterIsCoveredToo is the case a first application cannot
// see.
//
// The policies are applied by the migration to what exists that day. A
// partition created next month by the housekeeping loop is a table that did not
// exist then, and an uncovered one reads every tenant while the parent reads
// one.
func TestAPartitionCreatedLaterIsCoveredToo(t *testing.T) {
	tn := setupTenants(t)
	ctx := context.Background()

	// Far enough ahead that the migration cannot already have made them.
	if _, err := tn.ownerConn.Exec(ctx,
		`SELECT ensure_monthly_partitions('observation'::regclass, 9)`); err != nil {
		t.Fatalf("create partitions: %v", err)
	}

	var name string
	err := tn.ownerConn.QueryRow(ctx, `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = 'observation'::regclass
		 ORDER BY c.relname DESC LIMIT 1`).Scan(&name)
	if err != nil {
		t.Fatalf("find the newest partition: %v", err)
	}

	var enabled bool
	var policies int
	err = tn.ownerConn.QueryRow(ctx, `
		SELECT c.relrowsecurity, (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
		  FROM pg_class c WHERE c.relname = $1`, name).Scan(&enabled, &policies)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if !enabled || policies != 2 {
		t.Errorf("%s was created with rls=%v and %d policies: the door that creates partitions "+
			"is not the door that covers them", name, enabled, policies)
	}
}

// TestCreatingAPartitionTouchesNoOtherTable is the cost of the previous test
// being paid once rather than every month.
//
// Covering the new partition by re-applying the policies to everything would be
// five ACCESS EXCLUSIVE locks per table, held until the maintenance transaction
// commits, so the first tick of each month would block the whole application on
// asset_current and on asset. And the partition set only grows: after a couple
// of years that tick holds several hundred of those locks and starts failing on
// max_locks_per_transaction.
//
// Proved by taking a policy away from a table the tick has no business
// touching. If it comes back, the tick is touching everything.
func TestCreatingAPartitionTouchesNoOtherTable(t *testing.T) {
	tn := setupTenants(t)
	ctx := context.Background()

	exec(t, tn.ownerConn, `DROP POLICY tenant_isolation ON asset_current`)

	if _, err := tn.ownerConn.Exec(ctx,
		`SELECT ensure_monthly_partitions('observation'::regclass, 9)`); err != nil {
		t.Fatalf("create partitions: %v", err)
	}

	var restored int
	if err := tn.ownerConn.QueryRow(ctx, `
		SELECT count(*) FROM pg_policy p
		  JOIN pg_class c ON c.oid = p.polrelid
		 WHERE c.relname = 'asset_current' AND p.polname = 'tenant_isolation'`).Scan(&restored); err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if restored != 0 {
		t.Error("creating a partition re-applied the policy to asset_current, so the monthly tick " +
			"takes an exclusive lock on every tenant table at once")
	}

	// And the partition it did create is covered, or the assertion above is
	// satisfied by a tick that covers nothing at all.
	var name string
	var policies int
	err := tn.ownerConn.QueryRow(ctx, `
		SELECT c.relname, (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = 'observation'::regclass
		 ORDER BY c.relname DESC LIMIT 1`).Scan(&name, &policies)
	if err != nil {
		t.Fatalf("find the newest partition: %v", err)
	}
	if policies != 2 {
		t.Errorf("%s carries %d policies, want both", name, policies)
	}
}

// TestAPartitionReachedDirectlyIsScopedToo is the assertion that found the
// hole.
//
// Nothing in this repository names a partition. That is exactly the sort of
// statement row-level security is the last line for, and the parent's policy
// does not reach it.
func TestAPartitionReachedDirectlyIsScopedToo(t *testing.T) {
	tn := setupTenants(t)
	refuseToRunPrivileged(t, tn.appConn)

	var partition string
	err := tn.ownerConn.QueryRow(context.Background(), `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = 'observation'::regclass
		   AND EXISTS (SELECT 1 FROM observation o WHERE o.observed_at >= now() - interval '1 day')
		 ORDER BY c.relname LIMIT 1`).Scan(&partition)
	if err != nil {
		t.Fatalf("find a partition: %v", err)
	}

	through := asOrg(t, tn.appConn, tn.one, `SELECT count(*) FROM observation`)
	direct := asOrg(t, tn.appConn, tn.one, `SELECT count(*) FROM `+partition) //nolint:gosec // a catalog name

	if direct != through {
		t.Errorf("%s read directly returns %d rows and the parent returns %d: a partition without "+
			"its own policy reads every tenant", partition, direct, through)
	}
}
