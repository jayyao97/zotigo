package zotigod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type failRuntimeWALAppendStore struct {
	*zotigosession.FileStore
	failSequence uint64
}

func (s *failRuntimeWALAppendStore) AppendRuntimeWAL(ctx context.Context, sessionID string, record zotigosession.RuntimeWALRecord) error {
	if record.Sequence == s.failSequence {
		return errors.New("injected WAL append failure")
	}
	return s.FileStore.AppendRuntimeWAL(ctx, sessionID, record)
}

func TestRecoverRuntimeWALReplaysHistoryAndIgnoresCommittedStaleFile(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := agent.Snapshot{State: agent.StateIdle, History: []protocol.Message{protocol.NewUserMessage("question")}}
	sess := &zotigosession.Session{
		Metadata:        zotigosession.Metadata{ID: "session-replay", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		AgentSnapshot:   base,
		SnapshotVersion: 3,
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	header, err := zotigosession.NewRuntimeWALHeader(sess.ID, sess.SnapshotVersion, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	assistant := protocol.NewAssistantMessage("partial answer")
	if err := store.AppendRuntimeWAL(ctx, sess.ID, zotigosession.RuntimeWALRecord{
		WALID: header.WALID, Sequence: 1,
		Mutation: agent.HistoryMutation{Messages: []protocol.Message{assistant}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverRuntimeWAL(ctx, store, sess); err != nil {
		t.Fatal(err)
	}
	if got := sess.AgentSnapshot.History[len(sess.AgentSnapshot.History)-1].String(); got != "partial answer" {
		t.Fatalf("recovered history = %q", got)
	}
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverRuntimeWAL(ctx, store, sess); err != nil {
		t.Fatalf("stale committed WAL should be discarded: %v", err)
	}
	wal, err := store.LoadRuntimeWAL(ctx, sess.ID)
	if err != nil || wal != nil {
		t.Fatalf("stale WAL remains: wal=%#v err=%v", wal, err)
	}
}

func TestRecoverRuntimeWALPreservesCumulativeUsageAcrossReplacement(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	oldAssistant := protocol.NewAssistantMessage("old answer")
	oldAssistant.Metadata = &protocol.MessageMetadata{Usage: &protocol.Usage{InputTokens: 10, OutputTokens: 2}}
	base := agent.Snapshot{State: agent.StateIdle, History: []protocol.Message{
		protocol.NewUserMessage("old question"), oldAssistant,
	}}
	sess := &zotigosession.Session{
		Metadata:      zotigosession.Metadata{ID: "session-usage-replay", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		AgentSnapshot: base, SnapshotVersion: 1,
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	header, err := zotigosession.NewRuntimeWALHeader(sess.ID, sess.SnapshotVersion, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRuntimeWAL(ctx, sess.ID, zotigosession.RuntimeWALRecord{
		WALID: header.WALID, Sequence: 1,
		Mutation: agent.HistoryMutation{Replace: true, Messages: []protocol.Message{protocol.NewUserMessage("summary")}},
	}); err != nil {
		t.Fatal(err)
	}
	newAssistant := protocol.NewAssistantMessage("new answer")
	newAssistant.Metadata = &protocol.MessageMetadata{Usage: &protocol.Usage{InputTokens: 5, OutputTokens: 1}}
	if err := store.AppendRuntimeWAL(ctx, sess.ID, zotigosession.RuntimeWALRecord{
		WALID: header.WALID, Sequence: 2,
		Mutation: agent.HistoryMutation{Messages: []protocol.Message{newAssistant}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := recoverRuntimeWAL(ctx, store, sess); err != nil {
		t.Fatal(err)
	}
	want := (protocol.Usage{InputTokens: 15, OutputTokens: 3, TotalTokens: 18})
	if sess.AgentSnapshot.CumulativeUsage != want {
		t.Fatalf("cumulative usage = %+v, want %+v", sess.AgentSnapshot.CumulativeUsage, want)
	}
	if got := protocol.SessionUsage(sess.AgentSnapshot.History).Normalized(); got == want {
		t.Fatalf("test did not replace compacted usage: history usage=%+v", got)
	}
}

func TestRuntimeWALBeginAcceptsPersistedSemanticSnapshot(t *testing.T) {
	type providerMetadata struct {
		Second string `json:"second"`
		First  string `json:"first"`
	}
	message := protocol.NewAssistantMessage("answer")
	message.Metadata = &protocol.MessageMetadata{Raw: map[string]any{
		"provider":      providerMetadata{Second: "two", First: "one"},
		"large_integer": int64(9007199254740993),
	}}
	snapshot := agent.Snapshot{State: agent.StatePaused, History: []protocol.Message{message}}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &zotigosession.Session{
		Metadata:        zotigosession.Metadata{ID: "session-semantic-begin"},
		AgentSnapshot:   snapshot,
		SnapshotVersion: 1,
	}
	ctx := context.Background()
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	wal := newWorkerRuntimeWAL(store, sess.ID)
	if err := wal.Begin(ctx, persisted, snapshot, "turn-semantic-begin"); err != nil {
		t.Fatalf("begin WAL from semantically identical persisted snapshot: %v", err)
	}
}

func TestRecoverLegacyRuntimeWALReplaysStartedTool(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := agent.Snapshot{State: agent.StateIdle, History: []protocol.Message{protocol.NewUserMessage("question")}}
	sess := &zotigosession.Session{
		Metadata:        zotigosession.Metadata{ID: "session-legacy-recovery"},
		AgentSnapshot:   base,
		SnapshotVersion: 3,
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	header := zotigosession.RuntimeWALHeader{
		FormatVersion:       1,
		SessionID:           sess.ID,
		TurnID:              "turn-legacy-recovery",
		WALID:               "legacy-recovery-wal",
		BaseSnapshotVersion: persisted.SnapshotVersion,
		BaseSnapshotDigest:  zotigosession.SnapshotDigestForRuntimeWAL(persisted.AgentSnapshot, 1),
	}
	assistant := protocol.NewAssistantMessage("")
	assistant.AddToolCall(protocol.ToolCall{ID: "call-legacy", Name: "shell", Arguments: `{"command":"touch marker"}`})
	records := []zotigosession.RuntimeWALRecord{
		{
			FormatVersion: 1,
			WALID:         header.WALID,
			Sequence:      1,
			Mutation:      agent.HistoryMutation{Messages: []protocol.Message{assistant}},
		},
		{
			FormatVersion:        1,
			WALID:                header.WALID,
			Sequence:             2,
			ToolExecutionStarted: &zotigosession.RuntimeWALToolExecution{ToolCallID: "call-legacy", ToolName: "shell"},
		},
	}
	headerLine, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	data := append(headerLine, '\n')
	for index := range records {
		records[index].Checksum = legacyRuntimeWALRecordChecksum(records[index])
		line, err := json.Marshal(records[index])
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, append(line, '\n')...)
	}
	walPath := filepath.Join(store.RootDir(), "sessions", sess.ID+".json.runtime.jsonl")
	if err := os.WriteFile(walPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverRuntimeWAL(ctx, store, persisted)
	if err != nil || !recovered {
		t.Fatalf("recover legacy WAL: recovered=%v err=%v", recovered, err)
	}
	last := persisted.AgentSnapshot.History[len(persisted.AgentSnapshot.History)-1]
	if last.Content[0].ToolResult == nil || !strings.Contains(last.Content[0].ToolResult.Text, "outcome is unknown") {
		t.Fatalf("legacy started tool was not recovered as unknown: %#v", last)
	}
	if wal, err := store.LoadRuntimeWAL(ctx, sess.ID); err != nil || wal != nil {
		t.Fatalf("legacy WAL remains after recovery: wal=%#v err=%v", wal, err)
	}
	reloaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	last = reloaded.AgentSnapshot.History[len(reloaded.AgentSnapshot.History)-1]
	if last.Content[0].ToolResult == nil || !strings.Contains(last.Content[0].ToolResult.Text, "outcome is unknown") {
		t.Fatalf("legacy recovery was not persisted: %#v", last)
	}
}

func legacyRuntimeWALRecordChecksum(record zotigosession.RuntimeWALRecord) string {
	record.Checksum = ""
	data, _ := json.Marshal(record)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestRecoverUnansweredToolCallAddsNonExecutedResult(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assistant := protocol.NewAssistantMessage("")
	assistant.AddToolCall(protocol.ToolCall{ID: "call-1", Name: "shell", Arguments: `{"command":"touch marker"}`})
	sess := &zotigosession.Session{
		Metadata:      zotigosession.Metadata{ID: "session-unanswered", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		AgentSnapshot: agent.Snapshot{State: agent.StatePaused, History: []protocol.Message{protocol.NewUserMessage("q"), assistant}},
	}
	if err := recoverUnansweredToolCalls(context.Background(), store, sess); err != nil {
		t.Fatal(err)
	}
	last := sess.AgentSnapshot.History[len(sess.AgentSnapshot.History)-1]
	if last.Role != protocol.RoleTool || !last.Content[0].ToolResult.IsError || sess.AgentSnapshot.State != agent.StateIdle {
		t.Fatalf("unexpected recovery snapshot: %#v", sess.AgentSnapshot)
	}
}

func TestRecoverRuntimeWALMarksStartedToolOutcomeUnknown(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := agent.Snapshot{State: agent.StateIdle, History: []protocol.Message{protocol.NewUserMessage("q")}}
	sess := &zotigosession.Session{
		Metadata:        zotigosession.Metadata{ID: "session-started", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		AgentSnapshot:   base,
		SnapshotVersion: 1,
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	header, err := zotigosession.NewRuntimeWALHeader(sess.ID, sess.SnapshotVersion, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	assistant := protocol.NewAssistantMessage("")
	assistant.AddToolCall(protocol.ToolCall{ID: "call-started", Name: "shell", Arguments: `{"command":"touch marker"}`})
	if err := store.AppendRuntimeWAL(ctx, sess.ID, zotigosession.RuntimeWALRecord{
		WALID: header.WALID, Sequence: 1,
		Mutation: agent.HistoryMutation{Messages: []protocol.Message{assistant}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRuntimeWAL(ctx, sess.ID, zotigosession.RuntimeWALRecord{
		WALID: header.WALID, Sequence: 2,
		ToolExecutionStarted: &zotigosession.RuntimeWALToolExecution{ToolCallID: "call-started", ToolName: "shell"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverRuntimeWAL(ctx, store, sess); err != nil {
		t.Fatal(err)
	}
	last := sess.AgentSnapshot.History[len(sess.AgentSnapshot.History)-1]
	if last.Content[0].ToolResult == nil || !strings.Contains(last.Content[0].ToolResult.Text, "outcome is unknown") {
		t.Fatalf("started tool was not recovered as unknown: %#v", last)
	}
}

func TestRecoverRuntimeWALPreservesCompletedToolResult(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := agent.Snapshot{History: []protocol.Message{protocol.NewUserMessage("q")}}
	sess := &zotigosession.Session{
		Metadata:        zotigosession.Metadata{ID: "session-completed", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		AgentSnapshot:   base,
		SnapshotVersion: 1,
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	header, err := zotigosession.NewRuntimeWALHeader(sess.ID, 1, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	assistant := protocol.NewAssistantMessage("")
	assistant.AddToolCall(protocol.ToolCall{ID: "call-complete", Name: "read_file", Arguments: `{}`})
	result := protocol.NewTextToolResult("call-complete", "actual result", false)
	records := []zotigosession.RuntimeWALRecord{
		{WALID: header.WALID, Sequence: 1, Mutation: agent.HistoryMutation{Messages: []protocol.Message{assistant}}},
		{WALID: header.WALID, Sequence: 2, ToolExecutionStarted: &zotigosession.RuntimeWALToolExecution{ToolCallID: "call-complete", ToolName: "read_file"}},
		{WALID: header.WALID, Sequence: 3, Mutation: agent.HistoryMutation{Messages: []protocol.Message{protocol.NewToolMessage([]protocol.ToolResult{result})}}},
	}
	for _, record := range records {
		if err := store.AppendRuntimeWAL(ctx, sess.ID, record); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := recoverRuntimeWAL(ctx, store, sess)
	if err != nil || !recovered {
		t.Fatalf("recover completed WAL: recovered=%v err=%v", recovered, err)
	}
	last := sess.AgentSnapshot.History[len(sess.AgentSnapshot.History)-1]
	if len(last.Content) != 1 || last.Content[0].ToolResult == nil || last.Content[0].ToolResult.Text != "actual result" {
		t.Fatalf("completed result was replaced: %#v", last)
	}
}

func TestPoisonedRuntimeWALCannotCommitAwayStartedToolEvidence(t *testing.T) {
	fileStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fileStore.Close()
	store := &failRuntimeWALAppendStore{FileStore: fileStore, failSequence: 3}
	base := agent.Snapshot{History: []protocol.Message{protocol.NewUserMessage("q")}}
	sess := &zotigosession.Session{
		Metadata:        zotigosession.Metadata{ID: "session-poisoned", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		AgentSnapshot:   base,
		SnapshotVersion: 1,
	}
	ctx := context.Background()
	if err := fileStore.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	wal := &workerRuntimeWAL{store: store, sessionID: sess.ID}
	if err := wal.Begin(ctx, sess, base, "turn-poisoned"); err != nil {
		t.Fatal(err)
	}
	assistant := protocol.NewAssistantMessage("")
	assistant.AddToolCall(protocol.ToolCall{ID: "call-side-effect", Name: "shell", Arguments: `{}`})
	if err := wal.RecordHistory(agent.HistoryMutation{Messages: []protocol.Message{assistant}}); err != nil {
		t.Fatal(err)
	}
	if err := wal.RecordToolExecutionStarted(context.Background(), &agent.ToolCall{ToolCallID: "call-side-effect", Name: "shell"}); err != nil {
		t.Fatal(err)
	}
	result := protocol.NewTextToolResult("call-side-effect", "done", false)
	if err := wal.RecordHistory(agent.HistoryMutation{Messages: []protocol.Message{protocol.NewToolMessage([]protocol.ToolResult{result})}}); err == nil {
		t.Fatal("expected injected result append failure")
	}
	putCalled := false
	if err := wal.Commit(ctx, sess, func(context.Context, *zotigosession.Session) error {
		putCalled = true
		return nil
	}); err == nil {
		t.Fatal("poisoned WAL committed")
	}
	if putCalled || sess.CommittedRuntimeWALID != "" {
		t.Fatalf("poisoned WAL advanced snapshot: put=%v session=%#v", putCalled, sess)
	}
	loaded, err := fileStore.LoadRuntimeWAL(ctx, sess.ID)
	if err != nil || loaded == nil || len(loaded.Records) != 2 || loaded.Records[1].ToolExecutionStarted == nil {
		t.Fatalf("started-tool evidence was not retained: wal=%#v err=%v", loaded, err)
	}
	if _, err := recoverRuntimeWAL(ctx, fileStore, sess); err != nil {
		t.Fatal(err)
	}
	last := sess.AgentSnapshot.History[len(sess.AgentSnapshot.History)-1]
	if last.Content[0].ToolResult == nil || !strings.Contains(last.Content[0].ToolResult.Text, "outcome is unknown") {
		t.Fatalf("poisoned WAL did not recover unknown outcome: %#v", last)
	}
}
