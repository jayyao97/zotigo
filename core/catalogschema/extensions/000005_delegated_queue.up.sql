-- Older daemons must never dispatch delegated work as ordinary user input.
CREATE TABLE session_input_queue_extended (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  id TEXT NOT NULL,
  payload TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('queued', 'paused', 'sending', 'sent', 'cancelled', 'completion_queued', 'completion_paused', 'completion_sending', 'completion_sent', 'completion_cancelled', 'delegated_queued', 'delegated_paused', 'delegated_sending', 'delegated_sent', 'delegated_cancelled')),
  batch_id TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1
);
INSERT INTO session_input_queue_extended SELECT * FROM session_input_queue;
INSERT INTO sqlite_sequence(name,seq) SELECT 'session_input_queue_extended',seq FROM sqlite_sequence WHERE name='session_input_queue' AND NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='session_input_queue_extended');
UPDATE sqlite_sequence SET seq=MAX(seq, COALESCE((SELECT seq FROM sqlite_sequence WHERE name='session_input_queue'),0)) WHERE name='session_input_queue_extended';
DROP TABLE session_input_queue;
ALTER TABLE session_input_queue_extended RENAME TO session_input_queue;
CREATE UNIQUE INDEX session_input_queue_session_id_id ON session_input_queue(session_id,id);
CREATE INDEX idx_session_input_queue ON session_input_queue(session_id,state,sequence);
