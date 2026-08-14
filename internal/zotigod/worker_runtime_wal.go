package zotigod

import (
	"context"
	"fmt"
	"sync"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type workerRuntimeWAL struct {
	store     zotigosession.RuntimeWALStore
	sessionID string
	onError   func(error)

	mu        sync.Mutex
	header    *zotigosession.RuntimeWALHeader
	seq       uint64
	appendErr error
}

func newWorkerRuntimeWAL(store zotigosession.Store, sessionID string) *workerRuntimeWAL {
	walStore, ok := store.(zotigosession.RuntimeWALStore)
	if !ok {
		return nil
	}
	return &workerRuntimeWAL{store: walStore, sessionID: sessionID}
}

func (w *workerRuntimeWAL) Begin(ctx context.Context, sess *zotigosession.Session, snapshot agent.Snapshot, turnID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header != nil {
		return fmt.Errorf("runtime WAL is already active")
	}
	if zotigosession.SnapshotDigest(sess.AgentSnapshot) != zotigosession.SnapshotDigest(snapshot) {
		return fmt.Errorf("runtime WAL base snapshot does not match persisted session")
	}
	header, err := zotigosession.NewRuntimeWALHeader(w.sessionID, sess.SnapshotVersion, snapshot)
	if err != nil {
		return err
	}
	header.TurnID = turnID
	if err := w.store.BeginRuntimeWAL(ctx, header); err != nil {
		return err
	}
	w.header = &header
	w.seq = 0
	w.appendErr = nil
	return nil
}

func (w *workerRuntimeWAL) RecordHistory(mutation agent.HistoryMutation) error {
	return w.append(zotigosession.RuntimeWALRecord{Mutation: mutation})
}

func (w *workerRuntimeWAL) RecordToolExecutionStarted(_ context.Context, call *agent.ToolCall) error {
	if call == nil {
		return fmt.Errorf("tool call is required")
	}
	return w.append(zotigosession.RuntimeWALRecord{ToolExecutionStarted: &zotigosession.RuntimeWALToolExecution{
		ToolCallID: call.ToolCallID,
		ToolName:   call.Name,
		Arguments:  call.Arguments,
	}})
}

func (w *workerRuntimeWAL) append(record zotigosession.RuntimeWALRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header == nil {
		return fmt.Errorf("runtime WAL is not active")
	}
	if w.appendErr != nil {
		return w.appendErr
	}
	w.seq++
	record.WALID = w.header.WALID
	record.Sequence = w.seq
	if err := w.store.AppendRuntimeWAL(context.Background(), w.sessionID, record); err != nil {
		w.seq--
		w.appendErr = fmt.Errorf("append runtime WAL: %w", err)
		if w.onError != nil {
			w.onError(w.appendErr)
		}
		return w.appendErr
	}
	return nil
}

func (w *workerRuntimeWAL) Commit(ctx context.Context, sess *zotigosession.Session, put func(context.Context, *zotigosession.Session) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.appendErr != nil {
		return fmt.Errorf("runtime WAL is poisoned: %w", w.appendErr)
	}
	sess.SnapshotVersion++
	if w.header != nil {
		sess.CommittedRuntimeWALID = w.header.WALID
		sess.CommittedRuntimeWALSeq = w.seq
	}
	if err := put(ctx, sess); err != nil {
		return err
	}
	if w.header == nil {
		return nil
	}
	if err := w.store.DeleteRuntimeWAL(ctx, w.sessionID); err != nil {
		return err
	}
	w.header = nil
	w.seq = 0
	w.appendErr = nil
	return nil
}

func recoverRuntimeWAL(ctx context.Context, store zotigosession.Store, sess *zotigosession.Session) (bool, error) {
	walStore, ok := store.(zotigosession.RuntimeWALStore)
	if !ok {
		return false, nil
	}
	wal, err := walStore.LoadRuntimeWAL(ctx, sess.ID)
	if err != nil || wal == nil {
		return false, err
	}
	lastSeq := uint64(0)
	if len(wal.Records) > 0 {
		lastSeq = wal.Records[len(wal.Records)-1].Sequence
	}
	if sess.CommittedRuntimeWALID == wal.Header.WALID && sess.CommittedRuntimeWALSeq >= lastSeq {
		return true, walStore.DeleteRuntimeWAL(ctx, sess.ID)
	}
	if wal.Header.BaseSnapshotVersion != sess.SnapshotVersion {
		return true, fmt.Errorf("runtime WAL base version %d does not match snapshot version %d", wal.Header.BaseSnapshotVersion, sess.SnapshotVersion)
	}
	if wal.Header.BaseSnapshotDigest != zotigosession.SnapshotDigest(sess.AgentSnapshot) {
		return true, fmt.Errorf("runtime WAL base snapshot checksum mismatch")
	}
	for _, record := range wal.Records {
		if record.ToolExecutionStarted != nil {
			continue
		}
		if record.Mutation.Replace {
			sess.AgentSnapshot.History = append([]protocol.Message(nil), record.Mutation.Messages...)
		} else {
			sess.AgentSnapshot.History = append(sess.AgentSnapshot.History, record.Mutation.Messages...)
		}
		if record.Mutation.HasUserContextState {
			sess.AgentSnapshot.UserContextState = record.Mutation.UserContextState
		}
	}
	if calls := lastHistoryToolCalls(sess.AgentSnapshot.History); len(calls) > 0 {
		started := make(map[string]struct{})
		for _, record := range wal.Records {
			if record.ToolExecutionStarted != nil {
				started[record.ToolExecutionStarted.ToolCallID] = struct{}{}
			}
		}
		results := make([]protocol.ToolResult, 0, len(calls))
		for _, call := range calls {
			text := "The previous worker stopped before this tool execution began. The tool was not executed."
			if _, ok := started[call.ID]; ok {
				text = "The previous worker stopped after this tool started but before a durable result was recorded. The outcome is unknown and the operation may have produced side effects. Do not repeat it automatically; verify current state with a read-only operation or ask the user before retrying."
				if wal.Header.TurnID != "" {
					text = toolRecoveryMarker(wal.Header.TurnID, call.ID) + " " + text
				}
			}
			result := protocol.NewTextToolResult(call.ID, text, true)
			result.ToolName = call.Name
			results = append(results, result)
		}
		sess.AgentSnapshot.History = append(sess.AgentSnapshot.History, protocol.NewToolMessage(results))
	}
	sess.AgentSnapshot.State = agent.StateIdle
	sess.AgentSnapshot.PendingActions = nil
	sess.AgentSnapshot.DeferredActions = nil
	sess.SnapshotVersion++
	sess.CommittedRuntimeWALID = wal.Header.WALID
	sess.CommittedRuntimeWALSeq = lastSeq
	if err := store.Put(ctx, sess); err != nil {
		return true, fmt.Errorf("persist runtime WAL recovery: %w", err)
	}
	return true, walStore.DeleteRuntimeWAL(ctx, sess.ID)
}

func lastHistoryToolCalls(history []protocol.Message) []protocol.ToolCall {
	if len(history) == 0 || history[len(history)-1].Role != protocol.RoleAssistant {
		return nil
	}
	var calls []protocol.ToolCall
	for _, part := range history[len(history)-1].Content {
		if part.ToolCall != nil {
			calls = append(calls, *part.ToolCall)
		}
	}
	return calls
}
