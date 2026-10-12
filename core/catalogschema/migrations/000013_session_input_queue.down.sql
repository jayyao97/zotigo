-- reverse: create index "idx_session_input_queue" to table: "session_input_queue"
DROP INDEX `idx_session_input_queue`;
-- reverse: create index "session_input_queue_session_id_id" to table: "session_input_queue"
DROP INDEX `session_input_queue_session_id_id`;
-- reverse: create "session_input_queue" table
DROP TABLE `session_input_queue`;

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 12 WHERE singleton = 1;
