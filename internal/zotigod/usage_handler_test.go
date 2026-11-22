package zotigod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestUsageEndpoints(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// 18:00 UTC on the 19th is the 20th in Shanghai.
	at := time.Date(2026, 11, 19, 18, 0, 0, 0, time.UTC)
	for _, record := range []zotigosession.TurnUsageRecord{
		{SessionID: "a", TurnID: "1", Runtime: "zotigo", Model: "m", Status: "completed", CompletedAt: at, Usage: protocol.Usage{InputTokens: 10, CacheReadInputTokens: 90, OutputTokens: 5}},
		{SessionID: "b", TurnID: "1", Runtime: "codex", Model: "m", Status: "completed", Channel: true, CompletedAt: at, Usage: protocol.Usage{InputTokens: 3, OutputTokens: 1}},
	} {
		if err := store.RecordTurnUsage(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	h := &handler{store: store}
	serve := func(handle http.HandlerFunc, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handle(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	response := serve(h.handleUsageActivity, "/usage/activity?tz=Asia/Shanghai&from=2026-11-20&to=2026-11-20")
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	var activity struct {
		Data struct {
			Version int `json:"usage_query_version"`
			Range   struct {
				Total      int64 `json:"total"`
				CacheInput int64 `json:"cache_input"`
			} `json:"range"`
			Days []struct {
				Date string `json:"date"`
			} `json:"days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &activity); err != nil {
		t.Fatal(err)
	}
	if activity.Data.Version != 1 || activity.Data.Range.Total != 109 || activity.Data.Range.CacheInput != 103 || len(activity.Data.Days) != 1 || activity.Data.Days[0].Date != "2026-11-20" {
		t.Fatalf("activity %s", response.Body.String())
	}

	response = serve(h.handleUsageSessions, "/usage/sessions?tz=Asia/Shanghai&from=2026-11-20&to=2026-11-20&channel=only")
	var sessions struct {
		Data struct {
			Sessions []struct {
				SessionID string `json:"session_id"`
			} `json:"sessions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &sessions); err != nil || response.Code != http.StatusOK {
		t.Fatalf("%v %s", err, response.Body.String())
	}
	if len(sessions.Data.Sessions) != 1 || sessions.Data.Sessions[0].SessionID != "b" {
		t.Fatalf("sessions %s", response.Body.String())
	}

	for _, path := range []string{
		"/usage/activity?from=2026-11-20&to=2026-11-20",
		"/usage/activity?tz=Mars/Base&from=2026-11-20&to=2026-11-20",
		"/usage/activity?tz=Local&from=2026-11-20&to=2026-11-20",
		"/usage/activity?tz=UTC&from=2026-11-21&to=2026-11-20",
		"/usage/activity?tz=UTC&from=2025-01-01&to=2026-11-20",
		"/usage/activity?tz=UTC&from=2026-11-20&to=2026-11-20&channel=some",
		"/usage/sessions?tz=UTC&from=2026-11-20&to=2026-11-20&limit=0",
		"/usage/sessions?tz=UTC&from=2026-11-20&to=2026-11-20&cursor=%25%25",
	} {
		handle := h.handleUsageActivity
		if path[:15] == "/usage/sessions" {
			handle = h.handleUsageSessions
		}
		if response := serve(handle, path); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d %s", path, response.Code, response.Body.String())
		}
	}
}
