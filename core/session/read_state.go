package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReadState is shared by all clients. Version advances on a terminal turn or
// manual unread action, never on streamed deltas or metadata changes.
type ReadState struct {
	Version           int64 `json:"version"`
	ReadVersion       int64 `json:"read_version"`
	CompletedSequence int64 `json:"completed_sequence"`
	Unread            bool  `json:"unread"`
}

func (s *FileStore) SessionReadState(ctx context.Context, id string) (ReadState, error) {
	var state ReadState
	err := s.index.db.QueryRowContext(ctx, `SELECT version, read_version, completed_sequence FROM session_read_state WHERE session_id=?`, id).Scan(&state.Version, &state.ReadVersion, &state.CompletedSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	state.Unread = state.Version > state.ReadVersion
	return state, err
}

func (s *FileStore) ListSessionReadStates(ctx context.Context) (map[string]ReadState, error) {
	rows, err := s.index.db.QueryContext(ctx, `SELECT session_id, version, read_version, completed_sequence FROM session_read_state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	states := make(map[string]ReadState)
	for rows.Next() {
		var id string
		var state ReadState
		if err := rows.Scan(&id, &state.Version, &state.ReadVersion, &state.CompletedSequence); err != nil {
			return nil, err
		}
		state.Unread = state.Version > state.ReadVersion
		states[id] = state
	}
	return states, rows.Err()
}

// A delayed acknowledgement cannot clear a newer completion or manual unread.
// Neither operation changes activity timestamps or sidebar ordering.
func (s *FileStore) SetSessionReadState(ctx context.Context, id string, readVersion *int64) (ReadState, error) {
	if readVersion != nil && *readVersion < 0 {
		return ReadState{}, fmt.Errorf("negative read version")
	}
	tx, err := s.index.db.BeginTx(ctx, nil)
	if err != nil {
		return ReadState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_read_state(session_id) VALUES(?) ON CONFLICT DO NOTHING`, id); err != nil {
		return ReadState{}, err
	}
	if readVersion == nil {
		// Advance even if already unread: another client's pending auto-ack
		// must not erase this explicit action.
		_, err = tx.ExecContext(ctx, `UPDATE session_read_state SET version=version+1 WHERE session_id=?`, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE session_read_state SET read_version=MAX(read_version, MIN(version, ?)) WHERE session_id=?`, *readVersion, id)
	}
	if err != nil {
		return ReadState{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReadState{}, err
	}
	return s.SessionReadState(ctx, id)
}
