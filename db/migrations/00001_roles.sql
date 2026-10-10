-- Roles come before any business migration: retrofitting them means reassigning ownership
-- of every existing object, tedious and risky on a live database.
--
-- Why separate them:
--
--   1. Row-level security does not apply to a table's owner. An application connected as
--      owner would make it silently inoperative, the worst failure for an isolation
--      mechanism.
--   2. A role without DDL cannot drop a table, which bounds an SQL injection.
--   3. A REVOKE only works with two roles; an owner can grant privileges back to itself.
--
-- The schema knows nothing about authentication. These roles are NOLOGIN with no password:
-- LOGIN and a secret belong to the deployment entrypoint, as the method varies.

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

-- Without default privileges every new table needs a manual GRANT that gets forgotten, and
-- without the one on sequences bigserial columns fail obscurely.
--
-- No FOR ROLE clause: the default is the current role, migrations run as the owner, and
-- naming it would hard-code the deployment's choice.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO asm_app, asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO asm_app, asm_sys;

-- ANALYZE normally needs table ownership, and a non-owner's run is skipped with a warning,
-- leaving stale statistics invisible. MAINTAIN grants it and nothing else.
GRANT MAINTAIN ON ALL TABLES IN SCHEMA public TO asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT MAINTAIN ON TABLES TO asm_sys;

-- +goose Down

ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE MAINTAIN ON TABLES FROM asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE, SELECT ON SEQUENCES FROM asm_app, asm_sys;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM asm_app, asm_sys;

REVOKE USAGE ON SCHEMA public FROM asm_app, asm_sys;

-- Drops what the roles own and revokes what was granted to them in this database. Without
-- it DROP ROLE fails on any leftover privilege, making the migration irreversible.
DROP OWNED BY asm_app, asm_sys;

DROP ROLE IF EXISTS asm_app;
DROP ROLE IF EXISTS asm_sys;
