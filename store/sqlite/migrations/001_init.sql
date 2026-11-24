-- Migration 001: initial schema.
--
-- Two tables for v0.1:
--   singletons  small named string values (framework_id, schema_version, ...)
--   blobs       bucket-namespaced (key, value) pairs that typed wrappers
--               (apps, tasks) will sit on top of.
--
-- bucket gets an index because list-by-bucket is the only secondary lookup
-- we have today; PK on (bucket, key) covers point lookups for free.

CREATE TABLE IF NOT EXISTS singletons (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS blobs (
    bucket TEXT NOT NULL,
    key    TEXT NOT NULL,
    value  TEXT NOT NULL,
    PRIMARY KEY (bucket, key)
);

CREATE INDEX IF NOT EXISTS idx_blobs_bucket ON blobs(bucket);
