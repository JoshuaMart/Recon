-- Row-level security, and the third role that is allowed past it.
--
-- What is rejected is relying on a session variable alone, read by uniform
-- policies. A forgotten SET LOCAL then produces not an error but zero rows,
-- which is the silent failure the tenant guard was rewritten to remove. And a
-- component that legitimately serves every tenant would have no way to say so
-- except by not setting the variable, which is an omission and therefore
-- indistinguishable from forgetting one.
--
-- With two roles the property is structural: a component crosses tenants
-- because it connected with the role that allows it, which reads in the
-- deployment configuration rather than in the body of each query.

-- +goose Up

-- The organization of the current transaction, in one place.
--
-- NULLIF is the whole reason this is a function. SET LOCAL restores whatever
-- the session held before the transaction, and a custom variable never set at
-- session level holds '' rather than nothing, so current_setting(...)::uuid
-- returns zero rows on a connection's first transaction and raises "invalid
-- input syntax for type uuid" on its second. Copied into thirty policies, that
-- trap would be fixed in some of them.
--
-- +goose StatementBegin
CREATE FUNCTION tenant_org() RETURNS uuid
    LANGUAGE sql
    STABLE
    PARALLEL SAFE
    SET search_path = pg_catalog
AS $$
    SELECT NULLIF(current_setting('app.org_id', true), '')::uuid
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION tenant_org() IS
    'The organization set for this transaction, or null when none is. Null is what makes an '
    'unset connection return zero rows rather than raise, and rather than return everything.';

-- Which tables carry a policy is read from the catalog, never from a list.
--
-- A list kept by hand is exactly what forgets the table added last week, and
-- the doc says so about this mechanism specifically: the fallback moves the
-- property from the role to the policy, therefore to something a migration can
-- forget to carry onto a new table. So this is a function rather than a
-- sequence of statements, a later migration that adds a tenant table calls it
-- again, and a test walks the same catalog and fails on any table it missed.
--
-- +goose StatementBegin
CREATE FUNCTION apply_tenant_policies() RETURNS int
    LANGUAGE plpgsql
    SET search_path = public, pg_catalog
AS $$
DECLARE
    entry     record;
    predicate text;
    covered   int := 0;
BEGIN
    -- DROP POLICY IF EXISTS raises a notice per policy it did not find, which
    -- on a first application is two per table and buries the one line worth
    -- reading. Local to this transaction, so nothing else is quietened.
    PERFORM set_config('client_min_messages', 'warning', true);

    FOR entry IN
        SELECT c.relname
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public'
           AND c.relkind IN ('r', 'p')
           -- Partitions included, and it is not tidiness. A policy on the
           -- parent is applied to a partition reached through the parent and
           -- to nothing else: SELECT count(*) FROM observation_2026_08 as
           -- asm_app returns every tenant's rows while the same count through
           -- observation returns one tenant's. Nothing in this repository
           -- names a partition, which is exactly the sort of statement RLS is
           -- the last line for.
           AND (c.relname IN ('org', 'app_user')
                OR EXISTS (SELECT 1 FROM pg_attribute a
                            WHERE a.attrelid = c.oid AND a.attname = 'org_id'
                              AND a.attnum > 0 AND NOT a.attisdropped))
         ORDER BY c.relname
    LOOP
        predicate := CASE entry.relname
            -- The tenant carries its identity in id, so the policy that keeps
            -- an organization from reading another's name reads that column.
            WHEN 'org' THEN 'id = tenant_org()'
            -- A person belongs to several organizations, which is the whole
            -- point of the join table, so the only honest predicate goes
            -- through it. The consequence is worth naming: a role subject to
            -- this cannot insert a person, because the membership that would
            -- authorize the row does not exist yet. Creating people is
            -- bootstrap's job and bootstrap runs as the owner.
            WHEN 'app_user' THEN
                'EXISTS (SELECT 1 FROM membership m WHERE m.user_id = app_user.id '
                'AND m.org_id = tenant_org())'
            ELSE 'org_id = tenant_org()'
        END;

        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', entry.relname);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I FOR ALL TO asm_app USING (%s) WITH CHECK (%s)',
            entry.relname, predicate, predicate);

        -- The fallback for a cluster that will not grant BYPASSRLS, installed
        -- whether or not this one does. Installing only the path that happens
        -- to be available would make the other a code path no deployment ever
        -- runs, and the day it is needed is the day nobody can say whether it
        -- works. With both in place, taking the attribute away exercises this
        -- one for real, which is what the suite does.
        EXECUTE format('DROP POLICY IF EXISTS system_crosses ON %I', entry.relname);
        EXECUTE format(
            'CREATE POLICY system_crosses ON %I FOR ALL TO asm_sys USING (true) WITH CHECK (true)',
            entry.relname);

        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', entry.relname);
        covered := covered + 1;
    END LOOP;

    RETURN covered;
END
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION apply_tenant_policies() IS
    'Applies the tenant policy to every table carrying org_id, partitions included. '
    'A migration adding a tenant table calls this again.';

-- It creates policies, which is DDL, so it stays with the owner. PostgreSQL
-- grants EXECUTE to PUBLIC on a new function, so "not granted" is not a state
-- one starts in.
REVOKE EXECUTE ON FUNCTION apply_tenant_policies() FROM PUBLIC;

-- A partition created next month has to carry the policy too, and the only
-- thing that will be running then is the housekeeping loop. So the door that
-- creates partitions is the door that covers them: this is a replacement of
-- the function from 00002, identical but for the last three lines, because a
-- partition created without a policy is a table that reads every tenant and
-- nothing would say so.
--
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ensure_monthly_partitions(target regclass, months_ahead int DEFAULT 2)
RETURNS int
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    created int := 0;
    month   date;
    schema  text;
    parent  text;
    part    text;
BEGIN
    IF months_ahead < 0 THEN
        RAISE EXCEPTION 'months_ahead must not be negative, got %', months_ahead;
    END IF;

    SELECT n.nspname, c.relname
      INTO schema, parent
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = target;

    FOR i IN 0..months_ahead LOOP
        month := date_trunc('month', CURRENT_DATE)::date + (i || ' months')::interval;
        part  := format('%s_%s', parent, to_char(month, 'YYYY_MM'));

        IF to_regclass(format('%I.%I', schema, part)) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I.%I PARTITION OF %I.%I FOR VALUES FROM (%L) TO (%L)',
                schema, part, schema, parent, month, month + interval '1 month');
            created := created + 1;
        END IF;
    END LOOP;

    -- Only when something was created, so the ordinary tick that finds every
    -- partition already there stays the no-op it has always been.
    IF created > 0 THEN
        PERFORM apply_tenant_policies();
    END IF;

    RETURN created;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    covered int;
BEGIN
    SELECT apply_tenant_policies() INTO covered;
    RAISE NOTICE 'row-level security applied to % tables', covered;
END
$$;
-- +goose StatementEnd

-- BYPASSRLS needs a superuser to grant, which a managed database generally
-- does not give. This is therefore an attempt rather than a statement: a
-- cluster that refuses keeps the policy above and loses nothing, and the
-- refusal is said out loud rather than discovered on the day of a move.
--
-- +goose StatementBegin
DO $$
BEGIN
    ALTER ROLE asm_sys BYPASSRLS;
    RAISE NOTICE 'asm_sys carries BYPASSRLS';
EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'BYPASSRLS was refused, asm_sys crosses tenants through its policy instead';
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DO $$
DECLARE
    entry record;
BEGIN
    FOR entry IN
        SELECT DISTINCT c.relname
          FROM pg_policy p
          JOIN pg_class c ON c.oid = p.polrelid
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public'
           AND p.polname IN ('tenant_isolation', 'system_crosses')
    LOOP
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', entry.relname);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', entry.relname);
        EXECUTE format('DROP POLICY IF EXISTS system_crosses ON %I', entry.relname);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
    ALTER ROLE asm_sys NOBYPASSRLS;
EXCEPTION WHEN insufficient_privilege THEN
    NULL;
END
$$;
-- +goose StatementEnd

DROP FUNCTION IF EXISTS apply_tenant_policies();
DROP FUNCTION IF EXISTS tenant_org();
