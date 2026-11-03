-- Older workers cannot revalidate these deferred instructions.
DELETE FROM session_input_queue WHERE id GLOB 'completion:*';
DROP TABLE completion_triggers;
DELETE FROM metadata WHERE key='completion_trigger_key';
UPDATE schema_meta SET version = 15 WHERE singleton = 1;
