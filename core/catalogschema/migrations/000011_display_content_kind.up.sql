-- disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- create "new_display_items" table
CREATE TABLE `new_display_items` (`session_id` text NOT NULL, `sequence` integer NOT NULL, `message_at` integer NOT NULL, `dialogue` integer NOT NULL, `offset` integer NOT NULL, `length` integer NOT NULL, `content_kind` text NOT NULL DEFAULT 'unknown', PRIMARY KEY (`session_id`, `sequence`), CHECK (content_kind IN ('unknown', 'conversation', 'tool', 'event')));
-- copy rows from old table "display_items" to new temporary table "new_display_items"
INSERT INTO `new_display_items` (`session_id`, `sequence`, `message_at`, `dialogue`, `offset`, `length`) SELECT `session_id`, `sequence`, `message_at`, `dialogue`, `offset`, `length` FROM `display_items`;
-- drop "display_items" table after copying rows
DROP TABLE `display_items`;
-- rename temporary table "new_display_items" to "display_items"
ALTER TABLE `new_display_items` RENAME TO `display_items`;
-- create index "idx_display_time" to table: "display_items"
CREATE INDEX `idx_display_time` ON `display_items` (`session_id`, `message_at`, `sequence`);
-- create index "idx_display_dialogue_time" to table: "display_items"
CREATE INDEX `idx_display_dialogue_time` ON `display_items` (`session_id`, `dialogue`, `message_at`);
-- create index "idx_display_content_sequence" to table: "display_items"
CREATE INDEX `idx_display_content_sequence` ON `display_items` (`session_id`, `content_kind`, `sequence`, `message_at`);
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- Keep old binaries and read-only clients aware of the schema version.
-- JSONL owns message content. Existing batched maintenance recomputes the
-- classification and offsets; queries fail closed until each log is caught up.
DELETE FROM display_index_files;
UPDATE schema_meta SET version = 11 WHERE singleton = 1;
