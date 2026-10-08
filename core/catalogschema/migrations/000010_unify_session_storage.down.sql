-- reverse: create "display_index_files" table
DROP TABLE `display_index_files`;
-- reverse: create index "idx_display_dialogue_time" to table: "display_items"
DROP INDEX `idx_display_dialogue_time`;
-- reverse: create index "idx_display_time" to table: "display_items"
DROP INDEX `idx_display_time`;
-- reverse: create "display_items" table
DROP TABLE `display_items`;
-- reverse: create "metadata" table
DROP TABLE `metadata`;
-- reverse: create index "idx_session_images_session_id" to table: "session_images"
DROP INDEX `idx_session_images_session_id`;
-- reverse: create "session_images" table
DROP TABLE `session_images`;
-- reverse: create index "idx_sessions_working_directory_updated_at" to table: "sessions"
DROP INDEX `idx_sessions_working_directory_updated_at`;
-- reverse: create index "idx_sessions_updated_at" to table: "sessions"
DROP INDEX `idx_sessions_updated_at`;
-- reverse: create "sessions" table
DROP TABLE `sessions`;

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 9 WHERE singleton = 1;
