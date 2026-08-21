-- Pivots: the values that link assets nothing else connects.
--
-- Counting them on the fly means one COUNT per displayed value, which is
-- unworkable as soon as a page shows dozens. So the counter is maintained on
-- write, in the same transaction that rewrites the projection, which still
-- holds the old value at the moment of the diff.

-- +goose Up

CREATE TABLE pivot_count (
    org_id      uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    pivot_type  text NOT NULL,
    pivot_value text NOT NULL,
    count       int NOT NULL,
    PRIMARY KEY (org_id, pivot_type, pivot_value),
    CONSTRAINT pivot_count_type_known CHECK (
        pivot_type IN ('favicon', 'cert_spki', 'script', 'cookie_name')),
    -- The counter drifts upward, never downward: a pivot announced at 41 that
    -- links 12 sends somebody looking for thirty hosts that do not exist. A
    -- negative count means the decrement ran twice, and it must fail here
    -- rather than be discovered on a badge.
    CONSTRAINT pivot_count_not_negative CHECK (count >= 0)
);

-- One copy per distinct favicon, whatever the number of assets sharing it,
-- which is the property that matters since a shared favicon is the interesting
-- case. Not in the projection: one or two kilobytes per asset in the hottest
-- write path and in every search response is gigabytes on a large inventory,
-- and an image is neither a filter nor a pivot. It is the depiction of one.
CREATE TABLE favicon_image (
    org_id     uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    hash       text NOT NULL,
    media_type text NOT NULL,
    bytes      bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, hash),
    -- The value is chosen by the target, and nothing stops a server serving
    -- five megabytes under the name of a favicon. Past the bound the image is
    -- not stored: the hash and its counter keep working, only the thumbnail is
    -- missing, which is honest degradation rather than a surprise on a bill.
    CONSTRAINT favicon_image_bounded CHECK (octet_length(bytes) <= 65536)
);

-- Frequency on the internet, not inside the organization: PHPSESSID is noise
-- because it is universal, not because it is locally frequent. On a small
-- perimeter the variance dominates and a local threshold cannot tell a
-- framework cookie from a rare application one.
CREATE TABLE generic_pivot_value (
    pivot_type text NOT NULL,
    pattern    text NOT NULL,
    note       text,
    PRIMARY KEY (pivot_type, pattern),
    CONSTRAINT generic_pivot_type_known CHECK (
        pivot_type IN ('favicon', 'cert_spki', 'script', 'cookie_name'))
);

-- The table is the reflection of a repository file, never an autonomous
-- source. Only the seed writes, so an entry added by hand cannot survive a
-- deployment and make the divergence invisible.
REVOKE INSERT, UPDATE, DELETE ON generic_pivot_value FROM asm_app, asm_sys;

-- +goose Down

DROP TABLE IF EXISTS generic_pivot_value;
DROP TABLE IF EXISTS favicon_image;
DROP TABLE IF EXISTS pivot_count;
