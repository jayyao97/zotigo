-- Older workers cannot revalidate these deferred instructions.
DELETE FROM session_input_queue WHERE id GLOB 'completion:*';
DROP TABLE completion_triggers;
DELETE FROM metadata WHERE key='completion_trigger_key';
CREATE TABLE session_input_queue_base (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  id TEXT NOT NULL,
  payload TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('queued', 'paused', 'sending', 'sent', 'cancelled')),
  batch_id TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1
);
INSERT INTO session_input_queue_base SELECT * FROM session_input_queue;
INSERT INTO sqlite_sequence(name,seq) SELECT 'session_input_queue_base',seq FROM sqlite_sequence WHERE name='session_input_queue' AND NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='session_input_queue_base');
UPDATE sqlite_sequence SET seq=MAX(seq, COALESCE((SELECT seq FROM sqlite_sequence WHERE name='session_input_queue'),0)) WHERE name='session_input_queue_base';
DROP TABLE session_input_queue;
ALTER TABLE session_input_queue_base RENAME TO session_input_queue;
CREATE UNIQUE INDEX session_input_queue_session_id_id ON session_input_queue(session_id,id);
CREATE INDEX idx_session_input_queue ON session_input_queue(session_id,state,sequence);
