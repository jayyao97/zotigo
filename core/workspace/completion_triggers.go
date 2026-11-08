package workspace

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrTriggerUnavailable = errors.New("completion trigger expired, cancelled, or unavailable")
var ErrTriggerContentConflict = errors.New("completion trigger already accepted different callback text")

type CompletionTrigger struct {
	ID           string    `json:"id"`
	SessionID    string    `json:"session_id"`
	WorkspaceID  string    `json:"workspace_id"`
	Title        string    `json:"title"`
	OnFire       string    `json:"on_fire"`
	Policy       string    `json:"policy"`
	TTL          int       `json:"ttl"`
	ExpiresAt    time.Time `json:"expires_at"`
	State        string    `json:"state"`
	CallbackText string    `json:"callback_text,omitempty"`
	DeliveryMode string    `json:"delivery_mode,omitempty"`
}

// The signing key remains in the private catalog. Returned per-trigger tokens
// cannot authorize another trigger or any general daemon endpoint.
func (s *Store) completionToken(ctx context.Context, id string) (string, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('completion_trigger_key',?) ON CONFLICT(key) DO NOTHING`, base64.RawURLEncoding.EncodeToString(seed)); err != nil {
		return "", err
	}
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='completion_trigger_key'`).Scan(&encoded); err != nil {
		return "", err
	}
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if len(key) != 32 {
		return "", errors.New("invalid completion signing key")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Store) CompletionTrigger(ctx context.Context, id string) (CompletionTrigger, error) {
	var value CompletionTrigger
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload,state FROM completion_triggers WHERE id=?`, id).Scan(&payload, &value.State)
	if err != nil {
		return value, err
	}
	state := value.State
	err = json.Unmarshal([]byte(payload), &value)
	value.State = state
	if value.DeliveryMode == "" {
		value.DeliveryMode = "queue"
	}
	return value, err
}

func (s *Store) CreateCompletionTrigger(ctx context.Context, value CompletionTrigger) (CompletionTrigger, string, error) {
	if value.DeliveryMode == "" {
		value.DeliveryMode = "queue"
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	existing, err := s.CompletionTrigger(ctx, value.ID)
	if err == nil {
		if existing.SessionID != value.SessionID || existing.WorkspaceID != value.WorkspaceID || existing.Title != value.Title || existing.OnFire != value.OnFire || existing.Policy != value.Policy || existing.TTL != value.TTL || existing.DeliveryMode != value.DeliveryMode {
			return value, "", fmt.Errorf("completion trigger call ID conflict")
		}
		token, err := s.completionToken(ctx, value.ID)
		return existing, token, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return value, "", err
	}
	// Fired receipts remain the authority for paused queues and durable replay,
	// including after worker acknowledgement. Expiry only limits first delivery.
	if _, err = s.db.ExecContext(ctx, `DELETE FROM completion_triggers WHERE state<>'fired' AND expires_at<?`, time.Now().Add(-7*24*time.Hour).Unix()); err != nil {
		return value, "", err
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM completion_triggers WHERE session_id=? AND state='pending' AND expires_at>?`, value.SessionID, time.Now().Unix()).Scan(&count); err != nil {
		return value, "", err
	}
	if count >= 16 {
		return value, "", fmt.Errorf("at most 16 pending completion triggers per session")
	}
	value.State = "pending"
	payload, err := json.Marshal(value)
	if err != nil {
		return value, "", err
	}
	token, err := s.completionToken(ctx, value.ID)
	if err != nil {
		return value, "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO completion_triggers(id,session_id,payload,expires_at,state) VALUES(?,?,?,?,'pending')`, value.ID, value.SessionID, string(payload), value.ExpiresAt.Unix())
	return value, token, err
}

func (s *Store) AuthenticateCompletionTrigger(ctx context.Context, id, token string) (CompletionTrigger, error) {
	value, err := s.CompletionTrigger(ctx, id)
	if err != nil {
		return value, ErrTriggerUnavailable
	}
	expected, err := s.completionToken(ctx, id)
	if err != nil {
		return value, err
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return CompletionTrigger{}, ErrTriggerUnavailable
	}
	return value, nil
}

// Consumption and queue insertion commit together. A lost HTTP response can be
// retried without adding another queue entry, even after daemon restart.
func (s *Store) FireCompletionTrigger(ctx context.Context, id, callbackText string, payload []byte) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var state, sessionID, storedPayload string
	var expiry int64
	if err = tx.QueryRowContext(ctx, `SELECT state,session_id,expires_at,payload FROM completion_triggers WHERE id=?`, id).Scan(&state, &sessionID, &expiry, &storedPayload); err != nil {
		return false, err
	}
	var value CompletionTrigger
	if err := json.Unmarshal([]byte(storedPayload), &value); err != nil {
		return false, err
	}
	if state == "fired" {
		if value.CallbackText != callbackText {
			return false, ErrTriggerContentConflict
		}
		return false, nil
	}
	if state != "pending" || expiry <= time.Now().Unix() {
		return false, ErrTriggerUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_input_queue(session_id,id,payload,state) VALUES(?,?,?,'completion_queued')`, sessionID, "completion:"+id, string(payload)); err != nil {
		return false, err
	}
	value.CallbackText, value.State = callbackText, "fired"
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE completion_triggers SET state='fired',payload=? WHERE id=?`, string(encoded), id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) CancelCompletionTrigger(ctx context.Context, sessionID, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE completion_triggers SET state='cancelled' WHERE session_id=? AND id=? AND state='pending'`, sessionID, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrTriggerUnavailable
	}
	return err
}
func (s *Store) ListCompletionTriggers(ctx context.Context, sessionID string) ([]CompletionTrigger, error) {
	return s.listCompletionTriggers(ctx, sessionID, false, "")
}

func (s *Store) CompletionTriggerHistory(ctx context.Context, sessionID, targetID string) ([]CompletionTrigger, error) {
	return s.listCompletionTriggers(ctx, sessionID, true, targetID)
}

func (s *Store) listCompletionTriggers(ctx context.Context, sessionID string, history bool, targetID string) ([]CompletionTrigger, error) {
	now := time.Now().Unix()
	query := `SELECT payload,state FROM completion_triggers WHERE session_id=? AND state='pending' AND expires_at>? ORDER BY expires_at`
	args := []any{sessionID, now}
	if history {
		// Bound decoded payloads while retaining every live notification and an
		// explicitly requested old receipt. All three branches stay session-scoped.
		query = `SELECT payload,state FROM completion_triggers WHERE session_id=? AND id IN (
			SELECT id FROM completion_triggers WHERE session_id=? AND state='pending' AND expires_at>?
			UNION SELECT id FROM (SELECT id FROM completion_triggers WHERE session_id=? AND (state<>'pending' OR expires_at<=?) ORDER BY expires_at DESC,id LIMIT 100)
			UNION SELECT id FROM completion_triggers WHERE session_id=? AND id=?
		) ORDER BY CASE WHEN state='pending' AND expires_at>? THEN 0 ELSE 1 END,expires_at DESC,id`
		args = []any{sessionID, sessionID, now, sessionID, now, sessionID, targetID, now}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []CompletionTrigger{}
	for rows.Next() {
		var raw, state string
		if err := rows.Scan(&raw, &state); err != nil {
			return nil, err
		}
		var value CompletionTrigger
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, err
		}
		value.State = state
		if value.State == "pending" && !value.ExpiresAt.After(time.Now()) {
			value.State = "expired"
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
