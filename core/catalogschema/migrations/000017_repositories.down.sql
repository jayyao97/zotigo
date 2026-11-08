-- Metadata migration may have registered a cache that was never cloned. If no
-- workspace ever bound that replacement, restore its original local sources.
UPDATE sources SET registered=1 WHERE id IN (
  SELECT sr.source_id FROM source_repositories sr
  JOIN sources replacement ON replacement.id=sr.replacement_source_id
  WHERE replacement.registered=1
    AND NOT EXISTS(SELECT 1 FROM workspace_checkouts c WHERE c.source_id=replacement.id)
);
UPDATE sources SET registered=0 WHERE id IN (
  SELECT sr.replacement_source_id FROM source_repositories sr
  JOIN sources original ON original.id=sr.source_id
  WHERE original.registered=1
    AND NOT EXISTS(SELECT 1 FROM workspace_checkouts c WHERE c.source_id=sr.replacement_source_id)
);
-- Preserve both source identities and every Git directory. Already-bound
-- managed copies become ordinary local sources for schema-16 clients.
DROP TABLE source_repository_migrations;
ALTER TABLE workspace_checkouts DROP COLUMN used_cached_base;
ALTER TABLE workspace_checkouts DROP COLUMN base_synced_at;
DROP TABLE source_repositories;
DROP TABLE repositories;
UPDATE schema_meta SET version = 16 WHERE singleton = 1;
