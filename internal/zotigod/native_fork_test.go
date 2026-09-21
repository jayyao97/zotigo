package zotigod

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/services"
	session "github.com/jayyao97/zotigo/core/session"
)

func archiveForkFixture(t *testing.T, dir, name string, messages []protocol.Message) protocol.Message {
	t.Helper()
	path := filepath.Join(dir, name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if err := json.NewEncoder(file).Encode(message); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "[Previous conversation summary]\nsummary\n\nIf you need specific details from before compaction, read the full transcript at: " + path}}}
}

func checkpointSource(history []protocol.Message) *session.Session {
	snapshot := agent.Snapshot{History: history}
	return &session.Session{AgentSnapshot: snapshot, ForkPoints: map[string]session.ForkPoint{"turn": {HistoryLength: len(history), HistoryDigest: session.SnapshotDigest(snapshot)}}}
}

func TestNativeForkArchiveChain(t *testing.T) {
	dir := t.TempDir()
	original := []protocol.Message{protocol.NewUserMessage("old"), protocol.NewAssistantMessage("answer")}
	original[1].Content = append(original[1].Content, protocol.ContentPart{Type: protocol.ContentTypeReasoning, Text: "reason", Signature: "signature", EncryptedContent: "encrypted", ReasoningID: "reason-id"})
	original[1].Metadata = &protocol.MessageMetadata{Raw: map[string]any{"large": json.Number("9007199254740993")}}
	first := archiveForkFixture(t, dir, "transcript_1.jsonl", original)
	second := archiveForkFixture(t, dir, "transcript_2.jsonl", []protocol.Message{first, protocol.NewUserMessage("future secret")})
	source := checkpointSource(original)
	source.AgentSnapshot.History = []protocol.Message{second, protocol.NewUserMessage("new context")}
	recovered, err := nativeForkSnapshot(context.Background(), source, "turn", dir)
	if err != nil || session.SnapshotDigest(recovered) != source.ForkPoints["turn"].HistoryDigest {
		t.Fatalf("recover: %v", err)
	}
	if len(source.AgentSnapshot.History) != 2 || source.AgentSnapshot.History[0].Content[0].Text != second.Content[0].Text {
		t.Fatal("source mutated")
	}
	// A checkpoint taken after the first compaction must retain its summary,
	// rather than expanding the conversation further into a different context.
	afterFirst := checkpointSource([]protocol.Message{first, protocol.NewUserMessage("later")})
	third := archiveForkFixture(t, dir, "transcript_3.jsonl", afterFirst.AgentSnapshot.History)
	afterFirst.AgentSnapshot.History = []protocol.Message{third}
	if _, err := nativeForkSnapshot(context.Background(), afterFirst, "turn", dir); err != nil {
		t.Fatal(err)
	}
}

func TestNativeForkActualCompressor(t *testing.T) {
	dir := t.TempDir()
	var history []protocol.Message
	for range 10 {
		history = append(history, protocol.NewUserMessage(strings.Repeat("question ", 500)), protocol.NewAssistantMessage(strings.Repeat("answer ", 500)))
	}
	source := checkpointSource(history[:2])
	compressor := services.NewCompressor(services.CompressorConfig{TranscriptDir: dir, PreserveRatio: 0.2})
	compressed, result, err := compressor.ForceCompress(context.Background(), history)
	if err != nil || !result.Compressed || result.TranscriptPath == "" {
		t.Fatalf("compression: %+v %v", result, err)
	}
	source.AgentSnapshot.History = compressed
	recovered, err := nativeForkSnapshot(context.Background(), source, "turn", dir)
	if err != nil || session.SnapshotDigest(recovered) != source.ForkPoints["turn"].HistoryDigest {
		t.Fatalf("recover: %v", err)
	}
}

func TestNativeForkArchiveFailures(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "outside", "symlink", "cycle", "lossy", "legacy", "cancelled", "forged"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			original := []protocol.Message{protocol.NewUserMessage("old"), protocol.NewAssistantMessage("answer")}
			source := checkpointSource(original)
			summary := archiveForkFixture(t, dir, "transcript_1.jsonl", original)
			path := filepath.Join(dir, "transcript_1.jsonl")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte("{broken\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "outside":
				dir = t.TempDir()
			case "symlink":
				other := filepath.Join(dir, "transcript_2.jsonl")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "cycle":
				summary = archiveForkFixture(t, dir, "transcript_1.jsonl", []protocol.Message{summary})
			case "lossy":
				summary = archiveForkFixture(t, dir, "transcript_1.jsonl", []protocol.Message{protocol.NewUserMessage("shortened")})
			case "legacy":
				source.ForkPoints = nil
			case "cancelled":
				cancel()
			case "forged":
				summary.CreatedAt = original[0].CreatedAt
			}
			source.AgentSnapshot.History = []protocol.Message{summary}
			if _, err := nativeForkSnapshot(ctx, source, "turn", dir); err == nil {
				t.Fatal("accepted unverifiable archive")
			}
		})
	}
}

func TestNativeForkArchiveReadBudget(t *testing.T) {
	dir := t.TempDir()
	archiveForkFixture(t, dir, "transcript_1.jsonl", []protocol.Message{protocol.NewUserMessage("hello")})
	budget := int64(8)
	messageBudget := 100000
	if _, err := readNativeTranscript(context.Background(), dir, filepath.Join(dir, "transcript_1.jsonl"), &budget, &messageBudget); err == nil {
		t.Fatal("ignored read budget")
	}
	budget, messageBudget = nativeArchiveBudget, 0
	if _, err := readNativeTranscript(context.Background(), dir, filepath.Join(dir, "transcript_1.jsonl"), &budget, &messageBudget); err == nil {
		t.Fatal("ignored message budget")
	}
}

func TestNativeForkArchiveRebasedContext(t *testing.T) {
	dir := t.TempDir()
	oldContext := protocol.NewUserMessage("old runtime context")
	oldContext.Contextual = true
	original := []protocol.Message{oldContext, protocol.NewUserMessage("first"), protocol.NewAssistantMessage("first answer"), protocol.NewUserMessage("second"), protocol.NewAssistantMessage("second answer")}
	source := checkpointSource(original)
	summary := archiveForkFixture(t, dir, "transcript_1.jsonl", original[:3])
	newContext := protocol.NewUserMessage("future runtime context")
	newContext.Contextual = true
	source.AgentSnapshot.History = append([]protocol.Message{summary, newContext}, original[3:]...)
	source.AgentSnapshot.History = append(source.AgentSnapshot.History, protocol.NewUserMessage("future secret"))
	snapshot, err := nativeForkSnapshot(context.Background(), source, "turn", dir)
	if err != nil || session.SnapshotDigest(snapshot) != source.ForkPoints["turn"].HistoryDigest {
		t.Fatalf("recover rebased history: %v", err)
	}
}

func TestNativeForkCompactedHTTP(t *testing.T) {
	// The API must find archives using the same HOME layout as the worker,
	// even when its session store lives in a different directory.
	h, caller := completeForkFixture(t)
	dir := filepath.Join(os.Getenv("HOME"), ".zotigo", "sessions", "compacted")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source, err := h.store.Get(ctx, caller.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	want := source.ForkPoints["turn"].HistoryDigest
	archive := archiveForkFixture(t, dir, "transcript_1.jsonl", source.AgentSnapshot.History)
	source.AgentSnapshot.History = []protocol.Message{archive, protocol.NewUserMessage("future secret")}
	if err := h.store.Put(ctx, source); err != nil {
		t.Fatal(err)
	}
	var childID string
	for range 2 {
		req := httptest.NewRequest("POST", "/sessions/"+source.ID+"/fork", strings.NewReader(`{"request_id":"archive-http","through_turn_id":"turn"}`))
		response := httptest.NewRecorder()
		h.handleSessionFork(response, req, source.ID)
		if response.Code != 201 {
			t.Fatalf("fork: %d %s", response.Code, response.Body.String())
		}
		var envelope struct {
			Data Session `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if childID != "" && childID != envelope.Data.ID {
			t.Fatal("retry created a different child")
		}
		childID = envelope.Data.ID
		child, err := h.store.Get(ctx, childID)
		if err != nil || session.SnapshotDigest(child.AgentSnapshot) != want {
			t.Fatalf("persisted branch: %v", err)
		}
	}
	after, err := h.store.Get(ctx, source.ID)
	if err != nil || session.SnapshotDigest(after.AgentSnapshot) != session.SnapshotDigest(source.AgentSnapshot) {
		t.Fatalf("source changed: %v", err)
	}
}
