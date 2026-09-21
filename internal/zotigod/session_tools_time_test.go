package zotigod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/session"
)

func TestSessionToolsTimeWindowMatchesEarlierConversation(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	yesterday := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
	for _, stamp := range []time.Time{yesterday, yesterday.Add(24 * time.Hour)} {
		if _, err := h.store.AppendDisplayItem(ctx, caller.SessionID, session.DisplayItem{Type: session.DisplayItemAssistantMessage, CreatedAt: stamp, Turn: &session.DisplayTurn{ID: "historical-turn"}, Content: []session.DisplayContentPart{{Type: "text", Text: stamp.Format(time.RFC3339)}}}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := h.listToolSessions(ctx, caller, json.RawMessage(`{"activity_since":"2026-09-19T00:00:00Z","activity_until":"2026-09-20T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), caller.SessionID) || !strings.Contains(string(encoded), `"last_matched_message_at":"2026-09-19T01:00:00Z"`) {
		t.Fatalf("list=%s", encoded)
	}
	raw, _ := json.Marshal(map[string]any{"session_id": caller.SessionID, "since": "2026-09-19T00:00:00Z", "until": "2026-09-20T00:00:00Z"})
	result, err = h.readToolSession(ctx, caller, raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(result)
	if !strings.Contains(string(encoded), `"timestamp":"2026-09-19T01:00:00Z"`) || !strings.Contains(string(encoded), `"turn_id":"historical-turn"`) || strings.Contains(string(encoded), "2026-09-20T01") {
		t.Fatalf("read=%s", encoded)
	}
	for _, args := range []string{`{"activity_since":"yesterday"}`, `{"activity_since":"2026-09-20T00:00:00Z","activity_until":"2026-09-19T00:00:00Z"}`} {
		if _, err = h.listToolSessions(ctx, caller, json.RawMessage(args)); err == nil {
			t.Fatalf("invalid time accepted: %s", args)
		}
	}
}

func TestSessionToolTimeWindowRepresentableRange(t *testing.T) {
	for _, stamp := range []string{"0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z"} {
		if _, err := toolTimeWindow(stamp, ""); err == nil {
			t.Fatalf("overflowing since accepted: %s", stamp)
		}
		if _, err := toolTimeWindow("", stamp); err == nil {
			t.Fatalf("overflowing until accepted: %s", stamp)
		}
	}
	window, err := toolTimeWindow("2026-09-20T08:00:00.123456789+08:00", "2026-09-20T01:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Date(2026, 9, 20, 0, 0, 0, 123456789, time.UTC)
	if window.Since == nil || !window.Since.Equal(expected) {
		t.Fatalf("timezone lost: %+v", window)
	}
}

func TestSessionToolsListUsesIndexedMetadataNotRuntimeHistory(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	path := filepath.Join(h.sessionStoreRoot(), "sessions", caller.SessionID+".json")
	if err := os.WriteFile(path, []byte("unreadable runtime snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Get(context.Background(), caller.SessionID); err == nil {
		t.Fatal("expected corrupted authoritative snapshot")
	}
	result, err := h.listToolSessions(context.Background(), caller, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), caller.SessionID) || !strings.Contains(string(encoded), `"agent":"zotigo"`) {
		t.Fatalf("indexed summary=%s", encoded)
	}
}
