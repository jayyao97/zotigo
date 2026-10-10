-- add column "item_id" to table: "display_items"
ALTER TABLE `display_items` ADD COLUMN `item_id` text NOT NULL DEFAULT '';
-- add column "search_text" to table: "display_items"
ALTER TABLE `display_items` ADD COLUMN `search_text` text NOT NULL DEFAULT '';
-- create index "idx_display_item_id" to table: "display_items"
CREATE INDEX `idx_display_item_id` ON `display_items` (`session_id`, `item_id`);

-- Keep old binaries and read-only clients aware of the schema version.
-- Backfill derived search text in the existing bounded background batches.
DELETE FROM display_index_files;
UPDATE schema_meta SET version = 12 WHERE singleton = 1;
