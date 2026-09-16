package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("channel resource not found")

type Store struct {
	db        *sql.DB
	messageMu sync.Mutex
}

func Open(root string) (*Store, error) {
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home: %w", err)
		}
		root = filepath.Join(home, ".zotigo")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create zotigo directory: %w", err)
	}
	path := filepath.Join(root, "channels.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open channels database: %w", err)
	}
	// A single writer is sufficient for channel metadata and avoids competing
	// SQLite connections surfacing SQLITE_BUSY during adapter callbacks.
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure channels database: %w", err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("protect channels database: %w", err)
	}
	s := &Store{db: db}
	if err = s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS channel_connections (
 id TEXT PRIMARY KEY, provider TEXT NOT NULL, name TEXT NOT NULL, app_id TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'stopped', last_error TEXT NOT NULL DEFAULT '',
 bot_open_id TEXT NOT NULL DEFAULT '', bot_name TEXT NOT NULL DEFAULT '', allow_chat_ids TEXT NOT NULL DEFAULT '[]', owner_sender_ids TEXT NOT NULL DEFAULT '[]',
 agent_instructions TEXT NOT NULL DEFAULT '', approval_instructions TEXT NOT NULL DEFAULT '', review_all_tools INTEGER NOT NULL DEFAULT 0,
 progress_mode TEXT NOT NULL DEFAULT 'interactive_card',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_conversations (
 id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES channel_connections(id) ON DELETE CASCADE,
 chat_id TEXT NOT NULL, root_id TEXT NOT NULL DEFAULT '', thread_id TEXT NOT NULL DEFAULT '', chat_type TEXT NOT NULL DEFAULT 'group', chat_mode TEXT NOT NULL DEFAULT '', chat_name TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '',
 session_strategy TEXT NOT NULL DEFAULT 'topic', session_id TEXT NOT NULL DEFAULT '', workspace_id TEXT NOT NULL DEFAULT '',
 session_agent TEXT NOT NULL DEFAULT 'zotigo', profile_name TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', reasoning_effort TEXT NOT NULL DEFAULT '',
 enabled INTEGER NOT NULL DEFAULT 0,
 sender_policy TEXT NOT NULL DEFAULT 'selected', allowed_sender_ids TEXT NOT NULL DEFAULT '[]', observed_senders TEXT NOT NULL DEFAULT '[]',
 agent_mode TEXT NOT NULL DEFAULT 'inherit', agent_instructions TEXT NOT NULL DEFAULT '',
 approval_mode TEXT NOT NULL DEFAULT 'inherit', approval_instructions TEXT NOT NULL DEFAULT '', review_all_tools INTEGER,
 last_activity_at TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(connection_id, chat_id, root_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS channel_session_binding ON channel_conversations(session_id) WHERE session_id <> '';
CREATE TABLE IF NOT EXISTS channel_messages (
 id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES channel_connections(id) ON DELETE CASCADE,
 conversation_id TEXT NOT NULL REFERENCES channel_conversations(id) ON DELETE CASCADE,
 provider_message_id TEXT NOT NULL, parent_provider_message_id TEXT NOT NULL DEFAULT '', sender_id TEXT NOT NULL, sender_name TEXT NOT NULL DEFAULT '', text TEXT NOT NULL,
 mentioned_bot INTEGER NOT NULL, trigger_status TEXT NOT NULL, status_detail TEXT NOT NULL DEFAULT '', reply_message_id TEXT NOT NULL DEFAULT '',
 delivery_mode TEXT NOT NULL DEFAULT '', reply_mode TEXT NOT NULL DEFAULT '', cot_id TEXT NOT NULL DEFAULT '', processing_marker_id TEXT NOT NULL DEFAULT '', final_message_id TEXT NOT NULL DEFAULT '', projected_sequence INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 UNIQUE(connection_id, provider_message_id)
);
CREATE INDEX IF NOT EXISTS channel_messages_by_conversation ON channel_messages(conversation_id, created_at DESC);
`)
	if err != nil {
		return fmt.Errorf("migrate channels database: %w", err)
	}
	var replyColumn int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_messages') WHERE name='reply_message_id'`).Scan(&replyColumn); err != nil {
		return fmt.Errorf("inspect channels database: %w", err)
	}
	if replyColumn == 0 {
		if _, err := s.db.Exec(`ALTER TABLE channel_messages ADD COLUMN reply_message_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate channel reply receipt: %w", err)
		}
	}
	for _, migration := range []struct{ column, definition string }{
		{"progress_mode", "TEXT NOT NULL DEFAULT 'interactive_card'"},
		{"owner_sender_ids", "TEXT NOT NULL DEFAULT '[]'"},
	} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_connections') WHERE name=?`, migration.column).Scan(&count); err != nil {
			return fmt.Errorf("inspect channel connection schema: %w", err)
		}
		if count == 0 {
			if _, err := s.db.Exec(`ALTER TABLE channel_connections ADD COLUMN ` + migration.column + ` ` + migration.definition); err != nil {
				return fmt.Errorf("migrate channel connection %s: %w", migration.column, err)
			}
		}
	}
	for _, migration := range []struct{ column, definition string }{
		{"delivery_mode", "TEXT NOT NULL DEFAULT ''"},
		{"reply_mode", "TEXT NOT NULL DEFAULT ''"},
		{"cot_id", "TEXT NOT NULL DEFAULT ''"},
		{"processing_marker_id", "TEXT NOT NULL DEFAULT ''"},
		{"final_message_id", "TEXT NOT NULL DEFAULT ''"},
		{"projected_sequence", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_messages') WHERE name=?`, migration.column).Scan(&count); err != nil {
			return fmt.Errorf("inspect channel message schema: %w", err)
		}
		if count == 0 {
			if _, err := s.db.Exec(`ALTER TABLE channel_messages ADD COLUMN ` + migration.column + ` ` + migration.definition); err != nil {
				return fmt.Errorf("migrate channel message %s: %w", migration.column, err)
			}
		}
	}
	if err := s.migrateConversationThreadScope(); err != nil {
		return err
	}
	if err := s.migrateConversationRootScope(); err != nil {
		return err
	}
	var parentMessageColumn int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_messages') WHERE name='parent_provider_message_id'`).Scan(&parentMessageColumn); err != nil {
		return fmt.Errorf("inspect channel parent message schema: %w", err)
	}
	if parentMessageColumn == 0 {
		if _, err := s.db.Exec(`ALTER TABLE channel_messages ADD COLUMN parent_provider_message_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate channel parent message: %w", err)
		}
	}
	var chatNameColumn int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_conversations') WHERE name='chat_name'`).Scan(&chatNameColumn); err != nil {
		return fmt.Errorf("inspect channel conversation chat name: %w", err)
	}
	if chatNameColumn == 0 {
		if _, err := s.db.Exec(`ALTER TABLE channel_conversations ADD COLUMN chat_name TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate channel conversation chat name: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS channel_thread_binding ON channel_conversations(connection_id,chat_id,thread_id) WHERE thread_id <> ''`); err != nil {
		return fmt.Errorf("migrate channel thread binding index: %w", err)
	}
	for _, migration := range []struct{ column, definition string }{
		{"chat_mode", "TEXT NOT NULL DEFAULT ''"},
		{"session_strategy", "TEXT NOT NULL DEFAULT 'topic'"},
		{"sender_policy", "TEXT NOT NULL DEFAULT 'selected'"},
		{"session_agent", "TEXT NOT NULL DEFAULT 'zotigo'"},
		{"profile_name", "TEXT NOT NULL DEFAULT ''"},
		{"model", "TEXT NOT NULL DEFAULT ''"},
		{"reasoning_effort", "TEXT NOT NULL DEFAULT ''"},
	} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_conversations') WHERE name=?`, migration.column).Scan(&count); err != nil {
			return fmt.Errorf("inspect channel conversation schema: %w", err)
		}
		if count == 0 {
			if _, err := s.db.Exec(`ALTER TABLE channel_conversations ADD COLUMN ` + migration.column + ` ` + migration.definition); err != nil {
				return fmt.Errorf("migrate channel conversation %s: %w", migration.column, err)
			}
		}
	}
	return nil
}

func (s *Store) migrateConversationThreadScope() error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_conversations') WHERE name='thread_id'`).Scan(&count); err != nil {
		return fmt.Errorf("inspect channel conversation schema: %w", err)
	}
	if count != 0 {
		return nil
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable channel migration foreign keys: %w", err)
	}
	defer func() { _, _ = s.db.Exec(`PRAGMA foreign_keys=ON`) }()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin channel conversation migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`
ALTER TABLE channel_messages RENAME TO channel_messages_before_thread_scope;
ALTER TABLE channel_conversations RENAME TO channel_conversations_before_thread_scope;
CREATE TABLE channel_conversations (
 id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES channel_connections(id) ON DELETE CASCADE,
 chat_id TEXT NOT NULL, root_id TEXT NOT NULL DEFAULT '', thread_id TEXT NOT NULL DEFAULT '', chat_type TEXT NOT NULL DEFAULT 'group', display_name TEXT NOT NULL DEFAULT '',
 session_id TEXT NOT NULL DEFAULT '', workspace_id TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 0,
 allowed_sender_ids TEXT NOT NULL DEFAULT '[]', observed_senders TEXT NOT NULL DEFAULT '[]',
 agent_mode TEXT NOT NULL DEFAULT 'inherit', agent_instructions TEXT NOT NULL DEFAULT '',
 approval_mode TEXT NOT NULL DEFAULT 'inherit', approval_instructions TEXT NOT NULL DEFAULT '', review_all_tools INTEGER,
 last_activity_at TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(connection_id, chat_id, root_id)
);
INSERT INTO channel_conversations
(id,connection_id,chat_id,root_id,thread_id,chat_type,display_name,session_id,workspace_id,enabled,allowed_sender_ids,observed_senders,agent_mode,agent_instructions,approval_mode,approval_instructions,review_all_tools,last_activity_at,created_at,updated_at)
SELECT id,connection_id,chat_id,'','',chat_type,display_name,session_id,workspace_id,enabled,allowed_sender_ids,observed_senders,agent_mode,agent_instructions,approval_mode,approval_instructions,review_all_tools,last_activity_at,created_at,updated_at
FROM channel_conversations_before_thread_scope;
CREATE TABLE channel_messages (
 id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES channel_connections(id) ON DELETE CASCADE,
 conversation_id TEXT NOT NULL REFERENCES channel_conversations(id) ON DELETE CASCADE,
 provider_message_id TEXT NOT NULL, sender_id TEXT NOT NULL, sender_name TEXT NOT NULL DEFAULT '', text TEXT NOT NULL,
 mentioned_bot INTEGER NOT NULL, trigger_status TEXT NOT NULL, status_detail TEXT NOT NULL DEFAULT '', reply_message_id TEXT NOT NULL DEFAULT '',
 delivery_mode TEXT NOT NULL DEFAULT '', reply_mode TEXT NOT NULL DEFAULT '', cot_id TEXT NOT NULL DEFAULT '', processing_marker_id TEXT NOT NULL DEFAULT '', final_message_id TEXT NOT NULL DEFAULT '', projected_sequence INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 UNIQUE(connection_id, provider_message_id)
);
INSERT INTO channel_messages
(id,connection_id,conversation_id,provider_message_id,sender_id,sender_name,text,mentioned_bot,trigger_status,status_detail,reply_message_id,delivery_mode,reply_mode,cot_id,processing_marker_id,final_message_id,projected_sequence,created_at)
SELECT id,connection_id,conversation_id,provider_message_id,sender_id,sender_name,text,mentioned_bot,trigger_status,status_detail,reply_message_id,delivery_mode,reply_mode,cot_id,processing_marker_id,final_message_id,projected_sequence,created_at
FROM channel_messages_before_thread_scope;
DROP TABLE channel_messages_before_thread_scope;
DROP TABLE channel_conversations_before_thread_scope;
CREATE UNIQUE INDEX channel_session_binding ON channel_conversations(session_id) WHERE session_id <> '';
CREATE INDEX channel_messages_by_conversation ON channel_messages(conversation_id, created_at DESC);
`); err != nil {
		return fmt.Errorf("migrate channel conversation thread scope: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit channel conversation thread scope: %w", err)
	}
	return nil
}

func (s *Store) migrateConversationRootScope() error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channel_conversations') WHERE name='root_id'`).Scan(&count); err != nil {
		return fmt.Errorf("inspect channel conversation root schema: %w", err)
	}
	if count != 0 {
		return nil
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable channel root migration foreign keys: %w", err)
	}
	defer func() { _, _ = s.db.Exec(`PRAGMA foreign_keys=ON`) }()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin channel conversation root migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`
ALTER TABLE channel_messages RENAME TO channel_messages_before_root_scope;
ALTER TABLE channel_conversations RENAME TO channel_conversations_before_root_scope;
CREATE TABLE channel_conversations (
 id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES channel_connections(id) ON DELETE CASCADE,
 chat_id TEXT NOT NULL, root_id TEXT NOT NULL DEFAULT '', thread_id TEXT NOT NULL DEFAULT '', chat_type TEXT NOT NULL DEFAULT 'group', display_name TEXT NOT NULL DEFAULT '',
 session_id TEXT NOT NULL DEFAULT '', workspace_id TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 0,
 allowed_sender_ids TEXT NOT NULL DEFAULT '[]', observed_senders TEXT NOT NULL DEFAULT '[]',
 agent_mode TEXT NOT NULL DEFAULT 'inherit', agent_instructions TEXT NOT NULL DEFAULT '',
 approval_mode TEXT NOT NULL DEFAULT 'inherit', approval_instructions TEXT NOT NULL DEFAULT '', review_all_tools INTEGER,
 last_activity_at TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(connection_id, chat_id, root_id)
);
INSERT INTO channel_conversations
(id,connection_id,chat_id,root_id,thread_id,chat_type,display_name,session_id,workspace_id,enabled,allowed_sender_ids,observed_senders,agent_mode,agent_instructions,approval_mode,approval_instructions,review_all_tools,last_activity_at,created_at,updated_at)
SELECT id,connection_id,chat_id,CASE WHEN thread_id<>'' THEN 'legacy-thread:'||thread_id ELSE '' END,thread_id,chat_type,display_name,session_id,workspace_id,enabled,allowed_sender_ids,observed_senders,agent_mode,agent_instructions,approval_mode,approval_instructions,review_all_tools,last_activity_at,created_at,updated_at
FROM channel_conversations_before_root_scope;
CREATE TABLE channel_messages (
 id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES channel_connections(id) ON DELETE CASCADE,
 conversation_id TEXT NOT NULL REFERENCES channel_conversations(id) ON DELETE CASCADE,
 provider_message_id TEXT NOT NULL, sender_id TEXT NOT NULL, sender_name TEXT NOT NULL DEFAULT '', text TEXT NOT NULL,
 mentioned_bot INTEGER NOT NULL, trigger_status TEXT NOT NULL, status_detail TEXT NOT NULL DEFAULT '', reply_message_id TEXT NOT NULL DEFAULT '',
 delivery_mode TEXT NOT NULL DEFAULT '', reply_mode TEXT NOT NULL DEFAULT '', cot_id TEXT NOT NULL DEFAULT '', processing_marker_id TEXT NOT NULL DEFAULT '', final_message_id TEXT NOT NULL DEFAULT '', projected_sequence INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 UNIQUE(connection_id, provider_message_id)
);
INSERT INTO channel_messages
(id,connection_id,conversation_id,provider_message_id,sender_id,sender_name,text,mentioned_bot,trigger_status,status_detail,reply_message_id,delivery_mode,reply_mode,cot_id,processing_marker_id,final_message_id,projected_sequence,created_at)
SELECT id,connection_id,conversation_id,provider_message_id,sender_id,sender_name,text,mentioned_bot,trigger_status,status_detail,reply_message_id,delivery_mode,reply_mode,cot_id,processing_marker_id,final_message_id,projected_sequence,created_at
FROM channel_messages_before_root_scope;
DROP TABLE channel_messages_before_root_scope;
DROP TABLE channel_conversations_before_root_scope;
CREATE UNIQUE INDEX channel_session_binding ON channel_conversations(session_id) WHERE session_id <> '';
CREATE INDEX channel_messages_by_conversation ON channel_messages(conversation_id, created_at DESC);
CREATE UNIQUE INDEX channel_thread_binding ON channel_conversations(connection_id,chat_id,thread_id) WHERE thread_id <> '';
`); err != nil {
		return fmt.Errorf("migrate channel conversation root scope: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit channel conversation root scope: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func jsonText(value any) string { data, _ := json.Marshal(value); return string(data) }
func parseList(value string) []string {
	var result []string
	_ = json.Unmarshal([]byte(value), &result)
	if result == nil {
		result = []string{}
	}
	return result
}
func parseSenders(value string) []Sender {
	var result []Sender
	_ = json.Unmarshal([]byte(value), &result)
	if result == nil {
		result = []Sender{}
	}
	return result
}

func normalizeIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (s *Store) PutConnection(ctx context.Context, value Connection) (Connection, error) {
	now := time.Now().UTC()
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	value.UpdatedAt = now
	value.AllowChatIDs = normalizeIDs(value.AllowChatIDs)
	value.OwnerSenderIDs = normalizeIDs(value.OwnerSenderIDs)
	_, err := s.db.ExecContext(ctx, `INSERT INTO channel_connections
(id,provider,name,app_id,enabled,status,last_error,bot_open_id,bot_name,allow_chat_ids,owner_sender_ids,agent_instructions,approval_instructions,review_all_tools,progress_mode,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET provider=excluded.provider,name=excluded.name,app_id=excluded.app_id,
enabled=excluded.enabled,allow_chat_ids=excluded.allow_chat_ids,owner_sender_ids=excluded.owner_sender_ids,agent_instructions=excluded.agent_instructions,
approval_instructions=excluded.approval_instructions,review_all_tools=excluded.review_all_tools,progress_mode=excluded.progress_mode,updated_at=excluded.updated_at`,
		value.ID, value.Provider, value.Name, value.AppID, value.Enabled, value.Status, value.LastError, value.BotOpenID, value.BotName, jsonText(value.AllowChatIDs), jsonText(value.OwnerSenderIDs), value.AgentInstructions, value.ApprovalInstructions, value.ReviewAllTools, value.ProgressMode, value.CreatedAt.Format(time.RFC3339Nano), value.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Connection{}, fmt.Errorf("save channel connection: %w", err)
	}
	return s.GetConnection(ctx, value.ID)
}

func scanConnection(row interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	var enabled, review int
	var allow, owners, created, updated string
	err := row.Scan(&c.ID, &c.Provider, &c.Name, &c.AppID, &enabled, &c.Status, &c.LastError, &c.BotOpenID, &c.BotName, &allow, &owners, &c.AgentInstructions, &c.ApprovalInstructions, &review, &c.ProgressMode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Enabled = enabled != 0
	c.ReviewAllTools = review != 0
	c.AllowChatIDs = parseList(allow)
	c.OwnerSenderIDs = parseList(owners)
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if c.ProgressMode == "" {
		c.ProgressMode = ProgressModeInteractiveCard
	}
	return c, nil
}

const connectionColumns = `id,provider,name,app_id,enabled,status,last_error,bot_open_id,bot_name,allow_chat_ids,owner_sender_ids,agent_instructions,approval_instructions,review_all_tools,progress_mode,created_at,updated_at`

func (s *Store) GetConnection(ctx context.Context, id string) (Connection, error) {
	return scanConnection(s.db.QueryRowContext(ctx, `SELECT `+connectionColumns+` FROM channel_connections WHERE id=?`, id))
}
func (s *Store) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connectionColumns+` FROM channel_connections ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Connection
	for rows.Next() {
		c, e := scanConnection(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM channel_connections WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) SetConnectionRuntime(ctx context.Context, id, status, lastError, botID, botName string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_connections SET status=?,last_error=?,bot_open_id=?,bot_name=?,updated_at=? WHERE id=?`, status, lastError, botID, botName, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) SetConnectionOwners(ctx context.Context, id string, ownerIDs []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_connections SET owner_sender_ids=?,updated_at=? WHERE id=?`, jsonText(normalizeIDs(ownerIDs)), time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) EnsureConversation(ctx context.Context, connectionID, chatID, chatType string, at time.Time) (Conversation, error) {
	return s.EnsureConversationRoot(ctx, connectionID, chatID, "", "", chatType, at)
}

func (s *Store) EnsureGroupConversation(ctx context.Context, connectionID, chatID string) error {
	if connectionID == "" || chatID == "" {
		return errors.New("channel group requires connection and chat")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := conversationID(connectionID, chatID, "")
	_, err := s.db.ExecContext(ctx, `INSERT INTO channel_conversations(id,connection_id,chat_id,root_id,chat_type,last_activity_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(connection_id,chat_id,root_id) DO NOTHING`, id, connectionID, chatID, "", "group", now, now, now)
	return err
}

func (s *Store) PutGroupConversationName(ctx context.Context, connectionID, chatID, name string) error {
	name = strings.TrimSpace(name)
	if connectionID == "" || chatID == "" || name == "" {
		return errors.New("channel conversation name requires connection, chat, and name")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	id := conversationID(connectionID, chatID, "")
	if _, err = tx.ExecContext(ctx, `INSERT INTO channel_conversations(id,connection_id,chat_id,root_id,chat_type,chat_name,display_name,last_activity_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(connection_id,chat_id,root_id) DO UPDATE SET chat_name=excluded.chat_name,updated_at=excluded.updated_at`, id, connectionID, chatID, "", "group", name, name, now, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE channel_conversations SET chat_name=? WHERE connection_id=? AND chat_id=?`, name, connectionID, chatID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutGroupConversationMode(ctx context.Context, connectionID, chatID, mode string) error {
	mode = strings.TrimSpace(mode)
	if connectionID == "" || chatID == "" || (mode != ChatModeGroup && mode != ChatModeTopic) {
		return errors.New("channel conversation mode requires connection, chat, and a supported mode")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE channel_conversations SET chat_mode=? WHERE connection_id=? AND chat_id=?`, mode, connectionID, chatID)
	if err != nil {
		return err
	}
	if count, countErr := result.RowsAffected(); countErr != nil {
		return countErr
	} else if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) EnsureConversationRoot(ctx context.Context, connectionID, chatID, rootID, threadID, chatType string, at time.Time) (Conversation, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	id := conversationID(connectionID, chatID, rootID)
	err := s.db.QueryRowContext(ctx, `INSERT INTO channel_conversations(id,connection_id,chat_id,root_id,thread_id,chat_type,last_activity_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(connection_id,chat_id,root_id) DO UPDATE SET thread_id=CASE WHEN excluded.thread_id<>'' THEN excluded.thread_id ELSE channel_conversations.thread_id END,chat_type=excluded.chat_type,last_activity_at=excluded.last_activity_at,updated_at=excluded.updated_at RETURNING id`, id, connectionID, chatID, rootID, threadID, chatType, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)).Scan(&id)
	if err != nil {
		return Conversation{}, err
	}
	return s.GetConversation(ctx, id)
}

func conversationID(connectionID, chatID, rootID string) string {
	id := "conv_" + connectionID + "_" + chatID
	if rootID != "" {
		id += "_root_" + rootID
	}
	return id
}

func scanConversation(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	var enabled int
	var allowed, observed, last, created, updated string
	var review sql.NullBool
	err := row.Scan(&c.ID, &c.ConnectionID, &c.ChatID, &c.RootID, &c.ThreadID, &c.ChatType, &c.ChatMode, &c.ChatName, &c.DisplayName, &c.SessionStrategy, &c.SessionID, &c.WorkspaceID, &c.Agent, &c.ProfileName, &c.Model, &c.ReasoningEffort, &enabled, &c.SenderPolicy, &allowed, &observed, &c.AgentInstructionsMode, &c.AgentInstructions, &c.ApprovalInstructionsMode, &c.ApprovalInstructions, &review, &last, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Enabled = enabled != 0
	if c.Agent == "" {
		c.Agent = SessionAgentZotigo
	}
	if c.SessionStrategy == "" {
		c.SessionStrategy = SessionStrategyTopic
	}
	if c.SenderPolicy == "" {
		c.SenderPolicy = SenderPolicySelected
	}
	c.AllowedSenderIDs = parseList(allowed)
	c.ObservedSenders = parseSenders(observed)
	if review.Valid {
		v := review.Bool
		c.ReviewAllTools = &v
	}
	c.LastActivityAt, _ = time.Parse(time.RFC3339Nano, last)
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return c, nil
}

const conversationColumns = `id,connection_id,chat_id,root_id,thread_id,chat_type,chat_mode,chat_name,display_name,session_strategy,session_id,workspace_id,session_agent,profile_name,model,reasoning_effort,enabled,sender_policy,allowed_sender_ids,observed_senders,agent_mode,agent_instructions,approval_mode,approval_instructions,review_all_tools,last_activity_at,created_at,updated_at`

func (s *Store) GetConversation(ctx context.Context, id string) (Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM channel_conversations WHERE id=?`, id))
}
func (s *Store) GetConversationBySession(ctx context.Context, sessionID string) (Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM channel_conversations WHERE session_id=?`, sessionID))
}
func (s *Store) GetConversationByScope(ctx context.Context, connectionID, chatID, rootID string) (Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM channel_conversations WHERE connection_id=? AND chat_id=? AND root_id=?`, connectionID, chatID, rootID))
}
func (s *Store) ListConversations(ctx context.Context, connectionID string) ([]Conversation, error) {
	q := `SELECT ` + conversationColumns + ` FROM channel_conversations`
	args := []any{}
	if connectionID != "" {
		q += ` WHERE connection_id=?`
		args = append(args, connectionID)
	}
	q += ` ORDER BY last_activity_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Conversation
	for rows.Next() {
		c, e := scanConversation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) PutConversation(ctx context.Context, c Conversation) (Conversation, error) {
	c.AllowedSenderIDs = normalizeIDs(c.AllowedSenderIDs)
	if c.AgentInstructionsMode == "" {
		c.AgentInstructionsMode = OverrideInherit
	}
	if c.ApprovalInstructionsMode == "" {
		c.ApprovalInstructionsMode = OverrideInherit
	}
	if c.Agent == "" {
		c.Agent = SessionAgentZotigo
	}
	if c.SessionStrategy == "" {
		c.SessionStrategy = SessionStrategyTopic
	}
	if c.SenderPolicy == "" {
		c.SenderPolicy = SenderPolicySelected
	}
	now := time.Now().UTC()
	c.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE channel_conversations SET chat_mode=?,chat_name=?,display_name=?,session_strategy=?,session_id=?,workspace_id=?,session_agent=?,profile_name=?,model=?,reasoning_effort=?,enabled=?,sender_policy=?,allowed_sender_ids=?,agent_mode=?,agent_instructions=?,approval_mode=?,approval_instructions=?,review_all_tools=?,updated_at=? WHERE id=?`, c.ChatMode, c.ChatName, c.DisplayName, c.SessionStrategy, c.SessionID, c.WorkspaceID, c.Agent, c.ProfileName, c.Model, c.ReasoningEffort, c.Enabled, c.SenderPolicy, jsonText(c.AllowedSenderIDs), c.AgentInstructionsMode, c.AgentInstructions, c.ApprovalInstructionsMode, c.ApprovalInstructions, c.ReviewAllTools, now.Format(time.RFC3339Nano), c.ID)
	if err != nil {
		return Conversation{}, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return Conversation{}, err
	} else if count == 0 {
		return Conversation{}, ErrNotFound
	}
	updated, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM channel_conversations WHERE id=?`, c.ID))
	if err != nil {
		return Conversation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return updated, nil
}

func (s *Store) RecordMessage(ctx context.Context, connectionID string, in InboundMessage) (Message, error) {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM channel_messages WHERE connection_id=? AND provider_message_id=?`, connectionID, in.MessageID))
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Message{}, err
	}
	at := in.CreatedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	rootID := in.RootID
	if in.ChatType == "group" && rootID == "" {
		rootID = in.MessageID
	}
	groupConversationID := ""
	groupSessionStrategy := SessionStrategyTopic
	groupChatMode := ""
	if in.ChatType == "group" && rootID != "" {
		groupID := conversationID(connectionID, in.ChatID, "")
		_, err = tx.ExecContext(ctx, `INSERT INTO channel_conversations(id,connection_id,chat_id,root_id,chat_type,chat_name,display_name,last_activity_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(connection_id,chat_id,root_id) DO NOTHING`, groupID, connectionID, in.ChatID, "", in.ChatType, in.ChatName, in.ChatName, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
		if err != nil {
			return Message{}, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT id,session_strategy,chat_mode FROM channel_conversations WHERE connection_id=? AND chat_id=? AND root_id=''`, connectionID, in.ChatID).Scan(&groupConversationID, &groupSessionStrategy, &groupChatMode); err != nil {
			return Message{}, err
		}
		if in.ChatName != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE channel_conversations SET chat_name=? WHERE connection_id=? AND chat_id=?`, in.ChatName, connectionID, in.ChatID); err != nil {
				return Message{}, err
			}
		}
	}
	convID := ""
	currentRootID := ""
	if groupSessionStrategy == SessionStrategyShared {
		convID = groupConversationID
		_, err = tx.ExecContext(ctx, `UPDATE channel_conversations SET last_activity_at=?,updated_at=? WHERE id=?`, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), convID)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT id,root_id FROM channel_conversations WHERE connection_id=? AND chat_id=? AND (root_id=? OR (?<>'' AND thread_id=?)) ORDER BY CASE WHEN root_id=? THEN 0 ELSE 1 END LIMIT 1`, connectionID, in.ChatID, rootID, in.ThreadID, in.ThreadID, rootID).Scan(&convID, &currentRootID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		convID = conversationID(connectionID, in.ChatID, rootID)
		_, err = tx.ExecContext(ctx, `INSERT INTO channel_conversations(id,connection_id,chat_id,root_id,thread_id,chat_type,chat_mode,chat_name,display_name,last_activity_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, convID, connectionID, in.ChatID, rootID, in.ThreadID, in.ChatType, groupChatMode, in.ChatName, in.ConversationName, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	} else if err == nil && groupSessionStrategy != SessionStrategyShared {
		_, err = tx.ExecContext(ctx, `UPDATE channel_conversations SET root_id=?,thread_id=CASE WHEN ?<>'' THEN ? ELSE thread_id END,chat_mode=CASE WHEN chat_mode='' THEN ? ELSE chat_mode END,chat_name=CASE WHEN ?<>'' THEN ? ELSE chat_name END,display_name=CASE WHEN display_name='' THEN ? ELSE display_name END,last_activity_at=?,updated_at=? WHERE id=?`, rootID, in.ThreadID, in.ThreadID, groupChatMode, in.ChatName, in.ChatName, in.ConversationName, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), convID)
	}
	if err != nil {
		return Message{}, err
	}
	updateObservedSender := func(targetID string) error {
		var observed string
		if queryErr := tx.QueryRowContext(ctx, `SELECT observed_senders FROM channel_conversations WHERE id=?`, targetID).Scan(&observed); queryErr != nil {
			return queryErr
		}
		senders := parseSenders(observed)
		found := false
		changed := false
		for index, sender := range senders {
			if sender.ID == in.Sender.ID {
				found = true
				if sender.DisplayName == "" && in.Sender.DisplayName != "" {
					senders[index].DisplayName = in.Sender.DisplayName
					changed = true
				}
			}
		}
		if !found && in.Sender.ID != "" {
			senders = append(senders, in.Sender)
			changed = true
		}
		if !changed {
			return nil
		}
		_, updateErr := tx.ExecContext(ctx, `UPDATE channel_conversations SET observed_senders=? WHERE id=?`, jsonText(senders), targetID)
		return updateErr
	}
	if err = updateObservedSender(convID); err != nil {
		return Message{}, err
	}
	if in.ChatType == "group" && rootID != "" && groupConversationID != convID {
		if err = updateObservedSender(groupConversationID); err != nil {
			return Message{}, err
		}
	}
	m := Message{ID: "msg_" + connectionID + "_" + in.MessageID, ConnectionID: connectionID, ConversationID: convID, ProviderID: in.MessageID, ParentProviderID: in.ParentMessageID, Sender: in.Sender, Text: in.Text, MentionedBot: in.MentionedBot, TriggerStatus: "received", ReplyMode: in.ReplyMode, CreatedAt: at}
	_, err = tx.ExecContext(ctx, `INSERT INTO channel_messages(id,connection_id,conversation_id,provider_message_id,parent_provider_message_id,sender_id,sender_name,text,mentioned_bot,trigger_status,reply_mode,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(connection_id,provider_message_id) DO NOTHING`, m.ID, connectionID, convID, in.MessageID, in.ParentMessageID, in.Sender.ID, in.Sender.DisplayName, in.Text, in.MentionedBot, m.TriggerStatus, m.ReplyMode, at.Format(time.RFC3339Nano))
	if err != nil {
		return Message{}, err
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `DELETE FROM channel_messages WHERE conversation_id=? AND created_at<?`, convID, cutoff); err != nil {
		return Message{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM channel_messages WHERE conversation_id=? AND id NOT IN (SELECT id FROM channel_messages WHERE conversation_id=? ORDER BY created_at DESC,id DESC LIMIT 500)`, convID, convID); err != nil {
		return Message{}, err
	}
	if err = tx.Commit(); err != nil {
		return Message{}, err
	}
	return s.GetMessageByProviderID(ctx, connectionID, in.MessageID)
}

const messageColumns = `id,connection_id,conversation_id,provider_message_id,parent_provider_message_id,sender_id,sender_name,text,mentioned_bot,trigger_status,status_detail,reply_message_id,delivery_mode,reply_mode,cot_id,processing_marker_id,final_message_id,projected_sequence,created_at`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var mentioned int
	var projected int64
	var created string
	err := row.Scan(&m.ID, &m.ConnectionID, &m.ConversationID, &m.ProviderID, &m.ParentProviderID, &m.Sender.ID, &m.Sender.DisplayName, &m.Text, &mentioned, &m.TriggerStatus, &m.StatusDetail, &m.ReplyMessageID, &m.DeliveryMode, &m.ReplyMode, &m.COTID, &m.ProcessingMarkerID, &m.FinalMessageID, &projected, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	m.MentionedBot = mentioned != 0
	if projected > 0 {
		m.ProjectedSeq = uint64(projected)
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return m, err
}

func (s *Store) GetMessageByProviderID(ctx context.Context, connectionID, providerID string) (Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM channel_messages WHERE connection_id=? AND provider_message_id=?`, connectionID, providerID))
}
func (s *Store) SetMessageStatus(ctx context.Context, id, status, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET trigger_status=?,status_detail=? WHERE id=?`, status, detail, id)
	return err
}
func (s *Store) ClaimMessage(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET trigger_status='claimed',status_detail='' WHERE id=? AND trigger_status='received'`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}
func (s *Store) SetMessageProcessingMarker(ctx context.Context, id, markerID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET processing_marker_id=? WHERE id=? AND trigger_status='claimed'`, markerID, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("channel message claim was lost")
	}
	return nil
}
func (s *Store) SetMessageRunning(ctx context.Context, id string, receipt DeliveryReceipt) error {
	result, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET trigger_status='running',status_detail='',reply_message_id=?,delivery_mode=?,reply_mode=?,cot_id=?,processing_marker_id=? WHERE id=? AND trigger_status='claimed'`, receipt.MessageID, receipt.Mode, receipt.ReplyMode, receipt.COTID, receipt.ProcessingMarkerID, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("channel message claim was lost")
	}
	return nil
}
func (s *Store) SetMessageDeliveryUnknown(ctx context.Context, id string, receipt DeliveryReceipt, finalMessageID, detail string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET trigger_status='delivery_unknown',status_detail=?,reply_message_id=?,delivery_mode=?,reply_mode=?,cot_id=?,processing_marker_id=?,final_message_id=? WHERE id=? AND trigger_status IN ('claimed','running')`, detail, receipt.MessageID, receipt.Mode, receipt.ReplyMode, receipt.COTID, receipt.ProcessingMarkerID, finalMessageID, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("channel message claim was lost")
	}
	return nil
}
func (s *Store) SetMessageProjected(ctx context.Context, id string, sequence uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET projected_sequence=MAX(projected_sequence,?) WHERE id=? AND trigger_status='running'`, sequence, id)
	return err
}
func (s *Store) SetMessageFinalMessageID(ctx context.Context, id, finalMessageID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET final_message_id=? WHERE id=? AND trigger_status='running'`, finalMessageID, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("channel message was not running")
	}
	return nil
}
func (s *Store) SetMessageProcessed(ctx context.Context, id, finalMessageID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET trigger_status='processed',status_detail='',final_message_id=? WHERE id=? AND trigger_status='running'`, finalMessageID, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("channel message was not running")
	}
	return nil
}
func (s *Store) ListPendingMessages(ctx context.Context) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM channel_messages WHERE trigger_status IN ('received','claimed','running') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

// RecoverPendingMessages closes the previous daemon process's inbox states in
// one transaction before any adapter can accept events for this process.
func (s *Store) RecoverPendingMessages(ctx context.Context) ([]Message, error) {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+messageColumns+` FROM channel_messages WHERE trigger_status IN ('received','claimed','running') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	messages, err := scanMessages(rows)
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	for index := range messages {
		status, detail := "failed", "daemon_restarted_before_processing"
		switch messages[index].TriggerStatus {
		case "claimed":
			status, detail = "delivery_unknown", "daemon_restarted_after_possible_delivery"
		case "running":
			detail = "daemon_restarted_before_completion"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE channel_messages SET trigger_status=?,status_detail=? WHERE id=?`, status, detail, messages[index].ID); err != nil {
			return nil, err
		}
		messages[index].TriggerStatus = status
		messages[index].StatusDetail = detail
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return messages, nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		var mentioned int
		var projected int64
		var created string
		if err := rows.Scan(&m.ID, &m.ConnectionID, &m.ConversationID, &m.ProviderID, &m.ParentProviderID, &m.Sender.ID, &m.Sender.DisplayName, &m.Text, &mentioned, &m.TriggerStatus, &m.StatusDetail, &m.ReplyMessageID, &m.DeliveryMode, &m.ReplyMode, &m.COTID, &m.ProcessingMarkerID, &m.FinalMessageID, &projected, &created); err != nil {
			return nil, err
		}
		m.MentionedBot = mentioned != 0
		if projected > 0 {
			m.ProjectedSeq = uint64(projected)
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ClearMessageProcessingMarker(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_messages SET processing_marker_id='' WHERE id=?`, id)
	return err
}

func (s *Store) ListMessagesWithProcessingMarkers(ctx context.Context, connectionID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM channel_messages WHERE connection_id=? AND processing_marker_id<>'' AND trigger_status NOT IN ('received','claimed','running') ORDER BY created_at`, connectionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}
func (s *Store) ListMessages(ctx context.Context, conversationID string, limit int) ([]Message, error) {
	if err := s.PruneMessages(ctx, time.Now().UTC()); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM channel_messages WHERE conversation_id=? ORDER BY created_at DESC LIMIT ?`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

func (s *Store) PruneMessages(ctx context.Context, now time.Time) error {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM channel_messages WHERE created_at<?`, now.UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano))
	return err
}
