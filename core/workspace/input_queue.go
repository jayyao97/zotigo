package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrQueueConflict = errors.New("queued input changed or is already being sent")

// Older daemons only recognize the queue states they can safely dispatch.
// Preserve the host-specific prefix on disk while exposing ordinary API states.
type QueuedInput struct {
	Delegated bool            `json:"-"`
	ID        string          `json:"id"`
	Payload   json.RawMessage `json:"-"`
	State     string          `json:"state"`
	BatchID   string          `json:"-"`
	Revision  int64           `json:"revision"`
}

// QueuedSubmissionBoundary includes dispatched batches because admission gives
// a batch a new timestamp. The retained row holds the original submission time.
func (s *Store) QueuedSubmissionBoundary(ctx context.Context, sessionID string, earliest bool) (string, time.Time, error) {
	order := " DESC"
	if earliest {
		order = " ASC"
	}
	var id, stamp string
	err := s.db.QueryRowContext(ctx, `SELECT id,coalesce(json_extract(payload,'$.created_at'),'') FROM session_input_queue
		WHERE session_id=? AND state NOT IN ('cancelled','delegated_cancelled','completion_cancelled') ORDER BY sequence`+order+` LIMIT 1`, sessionID).Scan(&id, &stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil || stamp == "" {
		return id, time.Time{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return id, at, err
}

func (s *Store) IsQueuedBatch(ctx context.Context, sessionID, batchID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_input_queue WHERE session_id=? AND batch_id=?)`, sessionID, batchID).Scan(&exists)
	return exists, err
}

func (s *Store) QueuedInputByID(ctx context.Context, sessionID, id string) (*QueuedInput, error) {
	var item QueuedInput
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT id,payload,state,batch_id,revision FROM session_input_queue WHERE session_id=? AND id=?`, sessionID, id).Scan(&item.ID, &payload, &item.State, &item.BatchID, &item.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item.Payload = json.RawMessage(payload)
	item.Delegated = strings.HasPrefix(item.State, "delegated_")
	item.State = publicQueueState(item.State)
	return &item, nil
}

// Everything up to and including the underscore is the stored namespace. SQL
// transitions preserve it even for legacy client IDs resembling host IDs.
func publicQueueState(state string) string {
	return strings.TrimPrefix(strings.TrimPrefix(state, "completion_"), "delegated_")
}

func (s *Store) QueueInput(ctx context.Context, sessionID, id string, payload []byte) error {
	state := "queued"
	if strings.HasPrefix(id, "completion:") {
		state = "completion_queued"
	} else if strings.HasPrefix(id, "session_tool:") {
		state = "delegated_queued"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_input_queue(session_id,id,payload,state) VALUES(?,?,?,?) ON CONFLICT(session_id,id) DO NOTHING`, sessionID, id, string(payload), state)
	return err
}

func (s *Store) QueuedInputs(ctx context.Context, sessionID string) ([]QueuedInput, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,payload,state,batch_id,revision FROM session_input_queue WHERE session_id=? AND state IN ('queued','paused','sending','completion_queued','completion_paused','completion_sending','delegated_queued','delegated_paused','delegated_sending') ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []QueuedInput{}
	for rows.Next() {
		var item QueuedInput
		var payload string
		if err := rows.Scan(&item.ID, &payload, &item.State, &item.BatchID, &item.Revision); err != nil {
			return nil, err
		}
		item.Payload = json.RawMessage(payload)
		item.Delegated = strings.HasPrefix(item.State, "delegated_")
		item.State = publicQueueState(item.State)
		result = append(result, item)
	}
	return result, rows.Err()
}

// Revisions prevent stale editors from overwriting another client's changes.
func (s *Store) UpdateQueuedInput(ctx context.Context, sessionID, id string, revision int64, payload []byte, remove bool) error {
	var result sql.Result
	var err error
	if remove {
		result, err = s.db.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || 'cancelled',revision=revision+1 WHERE session_id=? AND id=? AND revision=? AND batch_id='' AND state IN ('queued','paused','completion_queued','completion_paused','delegated_queued','delegated_paused')`, sessionID, id, revision)
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE session_input_queue SET payload=?,revision=revision+1 WHERE session_id=? AND id=? AND revision=? AND batch_id='' AND state IN ('queued','paused','completion_queued','completion_paused','delegated_queued','delegated_paused')`, string(payload), sessionID, id, revision)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrQueueConflict
	}
	return err
}

// Claim freezes a complete batch before admission. Its ID survives uncertain
// worker acknowledgements, so retrying cannot start the same input twice.
func (s *Store) ClaimQueuedInputs(ctx context.Context, sessionID, batchID string, resume bool) ([]QueuedInput, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if resume {
		if _, err := tx.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || 'queued',revision=revision+1 WHERE session_id=? AND state IN ('paused','completion_paused','delegated_paused') AND batch_id=''`, sessionID); err != nil {
			return nil, err
		}
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT batch_id FROM session_input_queue WHERE session_id=? AND batch_id<>'' AND state IN ('queued','paused','sending','completion_queued','completion_paused','completion_sending','delegated_queued','delegated_paused','delegated_sending') ORDER BY sequence LIMIT 1`, sessionID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if existing != "" {
		if !resume {
			return nil, nil
		}
		batchID = existing
		if _, err := tx.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || 'sending',revision=revision+1 WHERE session_id=? AND batch_id=? AND state IN ('paused','completion_paused','delegated_paused')`, sessionID, batchID); err != nil {
			return nil, err
		}
	} else {
		var paused int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM session_input_queue WHERE session_id=? AND state IN ('paused','completion_paused','delegated_paused')`, sessionID).Scan(&paused); err != nil {
			return nil, err
		}
		if paused > 0 && !resume {
			return nil, nil
		}
		// Host-created messages keep their identity and authority when mixed
		// with ordinary queued input. Batch only the prefix before the next one.
		var first, firstState string
		if err := tx.QueryRowContext(ctx, `SELECT id,state FROM session_input_queue WHERE session_id=? AND batch_id='' AND state IN ('queued','paused','completion_queued','completion_paused','delegated_queued','delegated_paused') ORDER BY sequence LIMIT 1`, sessionID).Scan(&first, &firstState); errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		if firstState == "completion_queued" || firstState == "delegated_queued" {
			batchID = first
			_, err = tx.ExecContext(ctx, `UPDATE session_input_queue SET batch_id=?,state=replace(state,'queued','sending'),revision=revision+1 WHERE session_id=? AND id=?`, batchID, sessionID, first)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE session_input_queue SET batch_id=?,state='sending',revision=revision+1 WHERE session_id=? AND batch_id='' AND state IN ('queued','paused') AND sequence < COALESCE((SELECT MIN(sequence) FROM session_input_queue WHERE session_id=? AND batch_id='' AND state IN ('completion_queued','completion_paused','delegated_queued','delegated_paused')),9223372036854775807)`, batchID, sessionID, sessionID)
		}
		if err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,payload,state,batch_id,revision FROM session_input_queue WHERE session_id=? AND batch_id=? AND state IN ('paused','sending','completion_paused','completion_sending','delegated_paused','delegated_sending') ORDER BY sequence`, sessionID, batchID)
	if err != nil {
		return nil, err
	}
	result := []QueuedInput{}
	for rows.Next() {
		var item QueuedInput
		var payload string
		if err = rows.Scan(&item.ID, &payload, &item.State, &item.BatchID, &item.Revision); err != nil {
			break
		}
		item.Payload = json.RawMessage(payload)
		item.Delegated = strings.HasPrefix(item.State, "delegated_")
		item.State = publicQueueState(item.State)
		result = append(result, item)
	}
	scanErr := rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ClaimQueuedInput reserves one editable item without changing the other rows.
func (s *Store) ClaimQueuedInput(ctx context.Context, sessionID, id string, revision int64, batchID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || 'sending',batch_id=?,revision=revision+1 WHERE session_id=? AND id=? AND revision=? AND batch_id='' AND state IN ('queued','paused','completion_queued','completion_paused','delegated_queued','delegated_paused')`, batchID, sessionID, id, revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return ErrQueueConflict
	}
	return err
}

func (s *Store) FinishQueuedBatch(ctx context.Context, sessionID, batchID string, success bool) error {
	state := "paused"
	if success {
		state = "sent"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || ?,revision=revision+1 WHERE session_id=? AND batch_id=? AND state IN ('paused','sending','completion_paused','completion_sending','delegated_paused','delegated_sending')`, state, sessionID, batchID)
	return err
}

// A definite rejection is safe to edit; an uncertain acknowledgement is not.
func (s *Store) ReleaseQueuedBatch(ctx context.Context, sessionID, batchID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || 'queued',batch_id='',revision=revision+1 WHERE session_id=? AND batch_id=? AND state IN ('paused','sending','completion_paused','completion_sending','delegated_paused','delegated_sending')`, sessionID, batchID)
	return err
}

func (s *Store) PauseQueuedInputs(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || 'paused',revision=revision+1 WHERE session_id=? AND state IN ('queued','completion_queued','delegated_queued')`, sessionID)
	return err
}

func (s *Store) RecoverQueuedInputs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state=substr(state,1,instr(state,'_')) || 'paused',revision=revision+1 WHERE state IN ('queued','sending','completion_queued','completion_sending','delegated_queued','delegated_sending')`)
	return err
}
