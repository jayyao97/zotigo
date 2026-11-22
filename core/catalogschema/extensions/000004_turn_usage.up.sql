CREATE TABLE turn_usage (
  session_id TEXT NOT NULL,
  turn_id TEXT NOT NULL,
  runtime TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  is_channel INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  cache_read_tokens INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  total_tokens INTEGER NOT NULL,
  PRIMARY KEY (session_id, turn_id)
);
CREATE INDEX turn_usage_completed_at ON turn_usage(completed_at);
-- 15-minute UTC rollup, maintained with each turn, so activity reads do not
-- scale with the number of turns.
CREATE TABLE turn_usage_buckets (
  bucket INTEGER NOT NULL,
  model TEXT NOT NULL,
  runtime TEXT NOT NULL,
  is_channel INTEGER NOT NULL,
  turns INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  cache_read_tokens INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  total_tokens INTEGER NOT NULL,
  PRIMARY KEY (bucket, model, runtime, is_channel)
) WITHOUT ROWID;
