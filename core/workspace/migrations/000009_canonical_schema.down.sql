-- Canonical indexes/defaults remain compatible with schema 8 and preserve all data.
-- Do not restore divergent historical physical layouts when rolling back.
UPDATE schema_meta SET version = 8 WHERE singleton = 1;
