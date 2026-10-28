-- Let the older binary rebuild its own derived classification on rollback.
DELETE FROM display_index_files;
UPDATE schema_meta SET version = 14 WHERE singleton = 1;
