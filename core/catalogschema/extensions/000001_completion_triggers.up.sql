CREATE TABLE completion_triggers (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, payload TEXT NOT NULL, expires_at INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','fired','cancelled')));
CREATE INDEX completion_triggers_session ON completion_triggers(session_id,state,expires_at);
-- dev.31 must not run deferred callbacks without their authority checks.
-- Its queue queries only match queued/paused/sending. Ordinary rows retain
-- those states; new callbacks use completion_* states throughout their life.
CREATE TABLE session_input_queue_extended (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  id TEXT NOT NULL,
  payload TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('queued', 'paused', 'sending', 'sent', 'cancelled', 'completion_queued', 'completion_paused', 'completion_sending', 'completion_sent', 'completion_cancelled')),
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
