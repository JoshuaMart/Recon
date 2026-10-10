-- The suffix an ASM inventory actually asks. A service is keyed "app.target.com:443/tcp", so
-- ".target.com" is a suffix of the name, not the key: a suffix filter on key returns fqdn
-- rows and silently drops every service, most of an inventory and all of what anyone looks
-- for.
--
-- host answers "everything under this domain", so it gets key's treatment: reversing makes
-- the suffix a prefix, and reverse() is immutable, so it can be indexed.

-- +goose Up

CREATE INDEX asset_current_host_reversed_idx
    ON asset_current (reverse(host) text_pattern_ops)
    -- Partial, because an asset with no host is one this query can never be
    -- looking for, and skipping it costs nothing.
    WHERE host IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS asset_current_host_reversed_idx;
