-- create "session_read_state" table
CREATE TABLE `session_read_state` (`session_id` text NULL, `version` integer NOT NULL DEFAULT 0, `read_version` integer NOT NULL DEFAULT 0, `completed_sequence` integer NOT NULL DEFAULT 0, PRIMARY KEY (`session_id`), CHECK (read_version >= 0 AND version >= read_version AND completed_sequence >= 0));

-- Keep old binaries and read-only clients aware of the schema version.
-- Existing indexed history starts read; rebuilding its index must not generate
-- notifications for old turns. Future terminal records advance the version.
INSERT INTO session_read_state(session_id, completed_sequence)
SELECT session_id, MAX(sequence) FROM display_items GROUP BY session_id;
UPDATE schema_meta SET version = 14 WHERE singleton = 1;
