-- create "session_input_queue" table
CREATE TABLE `session_input_queue` (`sequence` integer NULL PRIMARY KEY AUTOINCREMENT, `session_id` text NOT NULL, `id` text NOT NULL, `payload` text NOT NULL, `state` text NOT NULL, `batch_id` text NOT NULL DEFAULT '', `revision` integer NOT NULL DEFAULT 1, CHECK (state IN ('queued', 'paused', 'sending', 'sent', 'cancelled')));
-- create index "session_input_queue_session_id_id" to table: "session_input_queue"
CREATE UNIQUE INDEX `session_input_queue_session_id_id` ON `session_input_queue` (`session_id`, `id`);
-- create index "idx_session_input_queue" to table: "session_input_queue"
CREATE INDEX `idx_session_input_queue` ON `session_input_queue` (`session_id`, `state`, `sequence`);

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 13 WHERE singleton = 1;
