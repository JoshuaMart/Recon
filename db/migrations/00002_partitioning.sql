-- Monthly partitioning, without an extension.
--
-- pg_partman is the right tool as soon as several partitioned tables with
-- different retentions are in play. Here there are two, on a trivial monthly
-- range, with a retention policy that reduces to a DROP TABLE. Thirty lines of
-- SQL against an extension to install, a non-official image to follow and one
-- more component at startup.
--
-- The functions come before the model because the model uses them.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION ensure_monthly_partitions(target regclass, months_ahead int DEFAULT 2)
RETURNS int
LANGUAGE plpgsql
-- The scheduler runs as the application role, which has no DDL, while creating
-- a partition is a CREATE TABLE. So this executes with its owner's rights, and
-- with a fixed search_path: a SECURITY DEFINER function that resolves names
-- through the caller's path is a way to run the owner's privileges on somebody
-- else's table.
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

    -- A partition belongs in its parent's schema, and saying so explicitly is
    -- what keeps the fixed search_path above from deciding where it lands:
    -- with pg_catalog first, an unqualified CREATE TABLE tries to write there
    -- and is refused, which is the right refusal for the wrong reason.
    SELECT n.nspname, c.relname
      INTO schema, parent
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = target;

    -- N+2 rather than N+1, so that an incident at the end of a month does not
    -- interrupt ingestion while somebody works out what broke.
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

    RETURN created;
END
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION ensure_monthly_partitions(regclass, int) IS
    'Creates this month''s partition and the next ones. Called by the maintenance loop, which starts unconditionally: it is not a feature.';

-- +goose StatementBegin
CREATE FUNCTION drop_monthly_partitions_before(target regclass, cutoff date)
RETURNS int
LANGUAGE plpgsql
AS $$
DECLARE
    dropped int := 0;
    part    record;
BEGIN
    FOR part IN
        SELECT c.oid::regclass AS name,
               (regexp_match(pg_get_expr(c.relpartbound, c.oid),
                             'FROM \(''([0-9-]+)'''))[1]::date AS starts
          FROM pg_class c
          JOIN pg_inherits i ON i.inhrelid = c.oid
         WHERE i.inhparent = target
    LOOP
        IF part.starts IS NOT NULL AND part.starts < date_trunc('month', cutoff) THEN
            EXECUTE format('DROP TABLE %s', part.name);
            dropped := dropped + 1;
        END IF;
    END LOOP;

    RETURN dropped;
END
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION drop_monthly_partitions_before(regclass, date) IS
    'Retention, as a DROP rather than a DELETE. Not granted to the application: purging is maintenance, run as the owner.';

-- PostgreSQL grants EXECUTE on a new function to PUBLIC, so "not granted" is
-- not a state a function starts in: without the revokes below, the application
-- role can call both of these and the grant that follows is decoration. Found
-- by calling the purge as asm_app and watching it run.
REVOKE EXECUTE ON FUNCTION ensure_monthly_partitions(regclass, int) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION drop_monthly_partitions_before(regclass, date) FROM PUBLIC;

-- Creating a partition is the only DDL the application is allowed, and only
-- through this door. The purge stays with the owner: retention is maintenance,
-- and a role that can drop a month of observations is a role that can lose the
-- one thing this system cannot reconstruct.
GRANT EXECUTE ON FUNCTION ensure_monthly_partitions(regclass, int) TO asm_app, asm_sys;

-- +goose Down

REVOKE EXECUTE ON FUNCTION ensure_monthly_partitions(regclass, int) FROM asm_app, asm_sys;
DROP FUNCTION IF EXISTS drop_monthly_partitions_before(regclass, date);
DROP FUNCTION IF EXISTS ensure_monthly_partitions(regclass, int);
