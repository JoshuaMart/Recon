-- The suffix an ASM inventory actually asks.
--
-- A service is keyed "app.target.com:443/tcp", so ".target.com" is a suffix of
-- the name and not of the key. A suffix filter on key therefore returns the
-- fqdn rows and silently drops every service, which is most of an inventory and
-- all of the part anybody is looking for.
--
-- host is the column that answers "everything under this domain", so it needs
-- the same treatment key already had: reversing turns the suffix into a prefix,
-- and reverse() is immutable, so it can be indexed.

-- +goose Up

CREATE INDEX asset_current_host_reversed_idx
    ON asset_current (reverse(host) text_pattern_ops)
    -- Partial, because an asset with no host is one this query can never be
    -- looking for, and skipping it costs nothing.
    WHERE host IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS asset_current_host_reversed_idx;
