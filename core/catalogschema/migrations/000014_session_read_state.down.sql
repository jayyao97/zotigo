-- reverse: create "session_read_state" table
DROP TABLE `session_read_state`;

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 13 WHERE singleton = 1;
