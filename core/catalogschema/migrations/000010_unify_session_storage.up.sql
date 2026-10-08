-- create "sessions" table
CREATE TABLE `sessions` (`id` text NULL, `working_directory` text NOT NULL, `agent` text NOT NULL DEFAULT 'zotigo', `profile_name` text NOT NULL DEFAULT '', `model` text NOT NULL DEFAULT '', `reasoning_effort` text NOT NULL DEFAULT '', `conversation_id` text NOT NULL DEFAULT '', `backend_version` text NOT NULL DEFAULT '', `backend_updated_at` integer NOT NULL DEFAULT 0, `backend_sync_version` integer NOT NULL DEFAULT 0, `approval_policy` text NOT NULL DEFAULT 'auto', `prompt_config` text NOT NULL DEFAULT '{}', `capabilities` text NOT NULL DEFAULT '{}', `last_prompt` text NOT NULL DEFAULT '', `created_at` integer NOT NULL, `updated_at` integer NOT NULL, PRIMARY KEY (`id`));
-- create index "idx_sessions_updated_at" to table: "sessions"
CREATE INDEX `idx_sessions_updated_at` ON `sessions` (`updated_at`);
-- create index "idx_sessions_working_directory_updated_at" to table: "sessions"
CREATE INDEX `idx_sessions_working_directory_updated_at` ON `sessions` (`working_directory`, `updated_at`);
-- create "session_images" table
CREATE TABLE `session_images` (`session_id` text NOT NULL, `name` text NOT NULL, `blob_path` text NOT NULL, `mime_type` text NOT NULL, `size_bytes` integer NOT NULL, `width` integer NOT NULL, `height` integer NOT NULL, `created_at` integer NOT NULL, PRIMARY KEY (`session_id`, `name`));
-- create index "idx_session_images_session_id" to table: "session_images"
CREATE INDEX `idx_session_images_session_id` ON `session_images` (`session_id`);
-- create "metadata" table
CREATE TABLE `metadata` (`key` text NULL, `value` text NOT NULL, PRIMARY KEY (`key`));
-- create "display_items" table
CREATE TABLE `display_items` (`session_id` text NOT NULL, `sequence` integer NOT NULL, `message_at` integer NOT NULL, `dialogue` integer NOT NULL, `offset` integer NOT NULL, `length` integer NOT NULL, PRIMARY KEY (`session_id`, `sequence`));
-- create index "idx_display_time" to table: "display_items"
CREATE INDEX `idx_display_time` ON `display_items` (`session_id`, `message_at`, `sequence`);
-- create index "idx_display_dialogue_time" to table: "display_items"
CREATE INDEX `idx_display_dialogue_time` ON `display_items` (`session_id`, `dialogue`, `message_at`);
-- create "display_index_files" table
CREATE TABLE `display_index_files` (`session_id` text NULL, `size` integer NOT NULL, `mtime` integer NOT NULL, `observed_size` integer NOT NULL DEFAULT -1, PRIMARY KEY (`session_id`));

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 10 WHERE singleton = 1;
