-- reverse: create index "idx_display_item_id" to table: "display_items"
DROP INDEX `idx_display_item_id`;
-- reverse: add column "search_text" to table: "display_items"
ALTER TABLE `display_items` DROP COLUMN `search_text`;
-- reverse: add column "item_id" to table: "display_items"
ALTER TABLE `display_items` DROP COLUMN `item_id`;

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 11 WHERE singleton = 1;
