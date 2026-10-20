package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var ErrQueueConflict = errors.New("queued input changed or is already being sent")

type QueuedInput struct {
	ID       string          `json:"id"`
	Payload  json.RawMessage `json:"-"`
	State    string          `json:"state"`
	BatchID  string          `json:"-"`
	Revision int64           `json:"revision"`
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
	return &item, nil
}

func (s *Store) QueueInput(ctx context.Context, sessionID, id string, payload []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_input_queue(session_id,id,payload,state) VALUES(?,?,?,'queued') ON CONFLICT(session_id,id) DO NOTHING`, sessionID, id, string(payload))
	return err
}

func (s *Store) QueuedInputs(ctx context.Context, sessionID string) ([]QueuedInput, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,payload,state,batch_id,revision FROM session_input_queue WHERE session_id=? AND state IN ('queued','paused','sending') ORDER BY sequence`, sessionID)
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
		result = append(result, item)
	}
	return result, rows.Err()
}

// Revisions prevent stale editors from overwriting another client's changes.
func (s *Store) UpdateQueuedInput(ctx context.Context, sessionID, id string, revision int64, payload []byte, remove bool) error {
	var result sql.Result
	var err error
	if remove {
		result, err = s.db.ExecContext(ctx, `UPDATE session_input_queue SET state='cancelled',revision=revision+1 WHERE session_id=? AND id=? AND revision=? AND batch_id='' AND state IN ('queued','paused')`, sessionID, id, revision)
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE session_input_queue SET payload=?,revision=revision+1 WHERE session_id=? AND id=? AND revision=? AND batch_id='' AND state IN ('queued','paused')`, string(payload), sessionID, id, revision)
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
		if _, err := tx.ExecContext(ctx, `UPDATE session_input_queue SET state='queued',revision=revision+1 WHERE session_id=? AND state='paused' AND batch_id=''`, sessionID); err != nil {
			return nil, err
		}
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT batch_id FROM session_input_queue WHERE session_id=? AND batch_id<>'' AND state IN ('queued','paused','sending') ORDER BY sequence LIMIT 1`, sessionID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if existing != "" {
		if !resume {
			return nil, nil
		}
		batchID = existing
		if _, err := tx.ExecContext(ctx, `UPDATE session_input_queue SET state='sending',revision=revision+1 WHERE session_id=? AND batch_id=? AND state='paused'`, sessionID, batchID); err != nil {
			return nil, err
		}
	} else {
		var paused int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM session_input_queue WHERE session_id=? AND state='paused'`, sessionID).Scan(&paused); err != nil {
			return nil, err
		}
		if paused > 0 && !resume {
			return nil, nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE session_input_queue SET batch_id=?,state='sending',revision=revision+1 WHERE session_id=? AND batch_id='' AND state IN ('queued','paused')`, batchID, sessionID); err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,payload,state,batch_id,revision FROM session_input_queue WHERE session_id=? AND batch_id=? AND state IN ('paused','sending') ORDER BY sequence`, sessionID, batchID)
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
	result, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state='sending',batch_id=?,revision=revision+1 WHERE session_id=? AND id=? AND revision=? AND batch_id='' AND state IN ('queued','paused')`, batchID, sessionID, id, revision)
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
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state=?,revision=revision+1 WHERE session_id=? AND batch_id=? AND state IN ('paused','sending')`, state, sessionID, batchID)
	return err
}

// A definite rejection is safe to edit; an uncertain acknowledgement is not.
func (s *Store) ReleaseQueuedBatch(ctx context.Context, sessionID, batchID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state='queued',batch_id='',revision=revision+1 WHERE session_id=? AND batch_id=? AND state IN ('paused','sending')`, sessionID, batchID)
	return err
}

func (s *Store) PauseQueuedInputs(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state='paused',revision=revision+1 WHERE session_id=? AND state='queued'`, sessionID)
	return err
}

func (s *Store) RecoverQueuedInputs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_input_queue SET state='paused',revision=revision+1 WHERE state IN ('queued','sending')`)
	return err
}
