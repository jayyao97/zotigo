-- Atlas's generated reversal referenced its renamed temporary table. Rebuild
-- the v10 representation explicitly, preserving every offset and timestamp.
CREATE TABLE display_items_v10 (
  session_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  message_at INTEGER NOT NULL,
  dialogue INTEGER NOT NULL,
  offset INTEGER NOT NULL,
  length INTEGER NOT NULL,
  PRIMARY KEY(session_id, sequence)
);
INSERT INTO display_items_v10(session_id,sequence,message_at,dialogue,offset,length)
SELECT session_id,sequence,message_at,dialogue,offset,length FROM display_items;
DROP TABLE display_items;
ALTER TABLE display_items_v10 RENAME TO display_items;
CREATE INDEX idx_display_time ON display_items(session_id, message_at, sequence);
CREATE INDEX idx_display_dialogue_time ON display_items(session_id, dialogue, message_at);

UPDATE schema_meta SET version = 10 WHERE singleton = 1;
