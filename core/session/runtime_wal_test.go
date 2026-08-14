package session

import (
	"context"
	"os"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestRuntimeWALRoundTripAndTornTail(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	snapshot := agent.Snapshot{History: []protocol.Message{protocol.NewUserMessage("question")}}
	header, err := NewRuntimeWALHeader("session-wal", 7, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	mutation := agent.HistoryMutation{Messages: []protocol.Message{protocol.NewAssistantMessage("answer")}}
	if err := store.AppendRuntimeWAL(ctx, header.SessionID, RuntimeWALRecord{WALID: header.WALID, Sequence: 1, Mutation: mutation}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(store.runtimeWALPath(header.SessionID), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"format_version":1`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	wal, err := store.LoadRuntimeWAL(ctx, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(wal.Records) != 1 || wal.Records[0].Mutation.Messages[0].String() != "answer" {
		t.Fatalf("unexpected WAL records: %#v", wal.Records)
	}
}

func TestRuntimeWALRejectsCorruptCompleteRecord(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	header, err := NewRuntimeWALHeader("session-corrupt", 0, agent.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(store.runtimeWALPath(header.SessionID), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("not-json\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := store.LoadRuntimeWAL(ctx, header.SessionID); err == nil {
		t.Fatal("expected complete corrupt WAL record to fail")
	}
}

func TestRuntimeWALKeepsValidFinalRecordWithoutNewline(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	header, err := NewRuntimeWALHeader("session-no-newline", 0, agent.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.BeginRuntimeWAL(ctx, header); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRuntimeWAL(ctx, header.SessionID, RuntimeWALRecord{
		WALID: header.WALID, Sequence: 1,
		Mutation: agent.HistoryMutation{Messages: []protocol.Message{protocol.NewUserMessage("durable")}},
	}); err != nil {
		t.Fatal(err)
	}
	path := store.runtimeWALPath(header.SessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-1], 0600); err != nil {
		t.Fatal(err)
	}
	wal, err := store.LoadRuntimeWAL(ctx, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(wal.Records) != 1 {
		t.Fatalf("valid final record was discarded: %#v", wal.Records)
	}
}
