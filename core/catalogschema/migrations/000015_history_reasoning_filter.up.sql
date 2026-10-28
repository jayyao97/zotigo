-- Recompute content classification from JSONL so reasoning-only messages no
-- longer consume conversation pages. Durable history and read state are kept.
DELETE FROM display_index_files;
UPDATE schema_meta SET version = 15 WHERE singleton = 1;
