-- Roles come before any business migration, because retrofitting them means
-- reassigning ownership of every existing object, which is tedious and risky
-- on a live database.
--
-- Three reasons for the separation, two of which go well beyond tidiness:
--
--   1. Row-level security does not apply to a table's owner. An application
--      connected as owner would make enabling it silently inoperative, which
--      is the worst failure mode an isolation mechanism can have: it signals
--      nothing.
--   2. A role without DDL cannot drop a table, which bounds an SQL injection.
--   3. A REVOKE only has an effect with two roles. An owner keeps its
--      privileges and can grant them back to itself.
--
-- The schema knows nothing about authentication. These roles are NOLOGIN and
-- carry no password: granting LOGIN and a secret belongs to the deployment
-- entrypoint, never to a migration file, because the method varies and the
-- schema has no business knowing it.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    -- Roles are cluster-scoped while migrations are per-database, so a second
    -- database in the same cluster reaches this having already created them.
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'asm_app') THEN
        CREATE ROLE asm_app NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'asm_sys') THEN
        CREATE ROLE asm_sys NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO asm_app, asm_sys;

-- Without default privileges, every new table would need a manual GRANT that
-- will be forgotten. Without the one on sequences, bigserial columns fail in a
-- way that is hard to diagnose.
--
-- No FOR ROLE clause: the default is the current role, migrations always run
-- as the owner, and naming it here would hard-code what the deployment picked.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO asm_app, asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO asm_app, asm_sys;

-- ANALYZE ordinarily requires table ownership, and run by a non-owner it is
-- skipped with a warning rather than an error, which is how stale statistics
-- stay invisible. MAINTAIN grants it without granting anything else.
GRANT MAINTAIN ON ALL TABLES IN SCHEMA public TO asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT MAINTAIN ON TABLES TO asm_sys;

-- +goose Down

ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE MAINTAIN ON TABLES FROM asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE, SELECT ON SEQUENCES FROM asm_app, asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM asm_app, asm_sys;

REVOKE USAGE ON SCHEMA public FROM asm_app, asm_sys;

-- Drops what the roles own and revokes what was granted to them in this
-- database. Without it, DROP ROLE fails on any leftover privilege, and a
-- migration that cannot be rolled back is one milestone 0 refuses.
DROP OWNED BY asm_app, asm_sys;

DROP ROLE IF EXISTS asm_app;
DROP ROLE IF EXISTS asm_sys;
