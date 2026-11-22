package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

func newUsageStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func recordUsage(t *testing.T, store *FileStore, record TurnUsageRecord) {
	t.Helper()
	if record.Runtime == "" {
		record.Runtime = "zotigo"
	}
	if record.Status == "" {
		record.Status = "completed"
	}
	if err := store.RecordTurnUsage(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func usageFilter(location *time.Location, from, to string) UsageFilter {
	start, _ := time.Parse(time.DateOnly, from)
	end, _ := time.Parse(time.DateOnly, to)
	return UsageFilter{Location: location, From: start, To: end, Channel: UsageChannelAll}
}

func TestRecordTurnUsageSkipsMissingUsageAndOverwritesRetries(t *testing.T) {
	store := newUsageStore(t)
	at := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "empty", CompletedAt: at})
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "t", Model: "m", CompletedAt: at, Usage: protocol.Usage{InputTokens: 1, OutputTokens: 1}})
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "t", Model: "m", CompletedAt: at, Usage: protocol.Usage{InputTokens: 10, CacheReadInputTokens: 30, OutputTokens: 5}})

	activity, err := store.UsageActivity(context.Background(), usageFilter(time.UTC, "2026-11-20", "2026-11-20"))
	if err != nil {
		t.Fatal(err)
	}
	want := UsageTotals{Total: 45, Input: 10, CacheRead: 30, Output: 5, CacheInput: 40, Turns: 1}
	if activity.Range != want || activity.AllTime != want {
		t.Fatalf("range %+v all-time %+v, want %+v", activity.Range, activity.AllTime, want)
	}
	if activity.TrackingSince == nil || !activity.TrackingSince.Equal(at) {
		t.Fatalf("tracking since = %v", activity.TrackingSince)
	}

	// A retry recorded on the next day moves the turn's share of the rollup.
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "t", Model: "m", Channel: true, CompletedAt: at.Add(24 * time.Hour), Usage: protocol.Usage{InputTokens: 2}})
	activity, err = store.UsageActivity(context.Background(), usageFilter(time.UTC, "2026-11-20", "2026-11-21"))
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Days) != 1 || activity.Days[0].Date != "2026-11-21" || activity.Range.Total != 2 || activity.Range.Turns != 1 {
		t.Fatalf("moved retry days=%+v range=%+v", activity.Days, activity.Range)
	}
	var buckets int
	if err := store.index.db.QueryRow(`SELECT COUNT(*) FROM turn_usage_buckets`).Scan(&buckets); err != nil || buckets != 1 {
		t.Fatalf("rollup rows = %d %v, want the emptied bucket removed", buckets, err)
	}
}

func TestUsageActivityBucketsByLocalDay(t *testing.T) {
	store := newUsageStore(t)
	// 23:30 UTC on the 19th is the 20th in Shanghai and 05:15 on the 20th in Kathmandu (+5:45).
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "late", Model: "m", CompletedAt: time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC), Usage: protocol.Usage{InputTokens: 100}})
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "early", Model: "m", CompletedAt: time.Date(2026, 11, 19, 18, 0, 0, 0, time.UTC), Usage: protocol.Usage{InputTokens: 7}})
	for _, tc := range []struct {
		zone string
		days map[string]int64
	}{
		{"UTC", map[string]int64{"2026-11-19": 107}},
		{"Asia/Shanghai", map[string]int64{"2026-11-20": 107}},
		{"Asia/Kathmandu", map[string]int64{"2026-11-19": 7, "2026-11-20": 100}},
		{"America/New_York", map[string]int64{"2026-11-19": 107}},
	} {
		location := mustLocation(t, tc.zone)
		activity, err := store.UsageActivity(context.Background(), usageFilter(location, "2026-11-18", "2026-11-21"))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, day := range activity.Days {
			got[day.Date] = day.Total
		}
		if len(got) != len(tc.days) {
			t.Fatalf("%s days = %v, want %v", tc.zone, got, tc.days)
		}
		for day, total := range tc.days {
			if got[day] != total {
				t.Fatalf("%s days = %v, want %v", tc.zone, got, tc.days)
			}
		}
	}
}

func TestUsageRangeHandlesDaylightSavingDay(t *testing.T) {
	store := newUsageStore(t)
	location := mustLocation(t, "America/New_York")
	// 2026-11-01 lasts 25 hours in New York; 23:30 local is 04:30 UTC on the 2nd.
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "t", Model: "m", CompletedAt: time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC), Usage: protocol.Usage{InputTokens: 3}})
	activity, err := store.UsageActivity(context.Background(), usageFilter(location, "2026-11-01", "2026-11-01"))
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Days) != 1 || activity.Days[0].Date != "2026-11-01" || activity.Range.Total != 3 {
		t.Fatalf("days %+v range %+v", activity.Days, activity.Range)
	}
}

func TestUsageDayWhoseMidnightIsSkipped(t *testing.T) {
	store := newUsageStore(t)
	location := mustLocation(t, "America/Santiago")
	// Clocks jump from 00:00 to 01:00 on 2026-09-06 (-04 to -03).
	for _, entry := range []struct {
		id string
		at time.Time
	}{
		{"before", time.Date(2026, 9, 6, 3, 30, 0, 0, time.UTC)}, // 23:30 -04 on 09-05
		{"inside", time.Date(2026, 9, 6, 4, 30, 0, 0, time.UTC)}, // 01:30 -03 on 09-06
		{"late", time.Date(2026, 9, 7, 2, 30, 0, 0, time.UTC)},   // 23:30 -03 on 09-06
		{"next", time.Date(2026, 9, 7, 3, 30, 0, 0, time.UTC)},   // 00:30 -03 on 09-07
	} {
		recordUsage(t, store, TurnUsageRecord{SessionID: entry.id, TurnID: "t", Model: "m", CompletedAt: entry.at, Usage: protocol.Usage{InputTokens: 1}})
	}
	filter := usageFilter(location, "2026-09-06", "2026-09-06")
	activity, err := store.UsageActivity(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Days) != 1 || activity.Days[0].Date != "2026-09-06" || activity.Range.Turns != 2 {
		t.Fatalf("days %+v range %+v", activity.Days, activity.Range)
	}
	page, err := store.UsageSessions(context.Background(), filter, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 2 || page.Sessions[0].SessionID != "inside" || page.Sessions[1].SessionID != "late" {
		t.Fatalf("sessions %+v", page.Sessions)
	}
	if start := localDayStart(2026, 9, 6, location); !start.Equal(time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("day start %v", start)
	}
}

func TestUsageCacheInputExcludesModelsWithoutCacheReads(t *testing.T) {
	store := newUsageStore(t)
	at := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	recordUsage(t, store, TurnUsageRecord{SessionID: "a", TurnID: "1", Model: "cached", CompletedAt: at, Usage: protocol.Usage{InputTokens: 10, CacheReadInputTokens: 80, CacheCreationInputTokens: 10, OutputTokens: 1}})
	recordUsage(t, store, TurnUsageRecord{SessionID: "a", TurnID: "2", Model: "plain", CompletedAt: at, Usage: protocol.Usage{InputTokens: 500, OutputTokens: 1}})
	recordUsage(t, store, TurnUsageRecord{SessionID: "b", TurnID: "1", Model: "plain", CompletedAt: at, Usage: protocol.Usage{InputTokens: 50}})
	filter := usageFilter(time.UTC, "2026-11-20", "2026-11-20")

	activity, err := store.UsageActivity(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Range.CacheInput != 100 || activity.Days[0].CacheInput != 100 || activity.AllTime.CacheInput != 100 {
		t.Fatalf("cache input range=%d day=%d all=%d, want 100", activity.Range.CacheInput, activity.Days[0].CacheInput, activity.AllTime.CacheInput)
	}
	for _, model := range activity.ByModel {
		want := int64(0)
		if model.Model == "cached" {
			want = 100
		}
		if model.CacheInput != want {
			t.Fatalf("model %s cache input %d, want %d", model.Model, model.CacheInput, want)
		}
	}
	page, err := store.UsageSessions(context.Background(), filter, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 2 || page.Sessions[0].SessionID != "a" || page.Sessions[0].CacheInput != 100 || page.Sessions[1].CacheInput != 0 {
		t.Fatalf("sessions %+v", page.Sessions)
	}
	if got := page.Sessions[0].Models; len(got) != 2 || got[0] != "cached" || got[1] != "plain" {
		t.Fatalf("models %v", got)
	}
}

func TestUsageModelCacheInputIgnoresRuntime(t *testing.T) {
	store := newUsageStore(t)
	at := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	recordUsage(t, store, TurnUsageRecord{SessionID: "a", TurnID: "1", Runtime: "zotigo", Model: "m", CompletedAt: at, Usage: protocol.Usage{InputTokens: 10, CacheReadInputTokens: 30}})
	recordUsage(t, store, TurnUsageRecord{SessionID: "b", TurnID: "1", Runtime: "codex", Model: "m", CompletedAt: at, Usage: protocol.Usage{InputTokens: 60}})
	activity, err := store.UsageActivity(context.Background(), usageFilter(time.UTC, "2026-11-20", "2026-11-20"))
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, model := range activity.ByModel {
		sum += model.CacheInput
	}
	if activity.Range.CacheInput != 100 || sum != activity.Range.CacheInput {
		t.Fatalf("range cache input %d, by-model sum %d, want 100 for both", activity.Range.CacheInput, sum)
	}
}

func TestUsageFiltersByModelAndChannel(t *testing.T) {
	store := newUsageStore(t)
	at := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "desk", Model: "m1", CompletedAt: at, Usage: protocol.Usage{InputTokens: 1}})
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "im", Model: "m1", Channel: true, CompletedAt: at, Usage: protocol.Usage{InputTokens: 10}})
	recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: "other", Model: "m2", CompletedAt: at, Usage: protocol.Usage{InputTokens: 100}})
	for _, tc := range []struct {
		model   string
		channel UsageChannelFilter
		want    int64
	}{
		{"", UsageChannelAll, 111},
		{"", UsageChannelOnly, 10},
		{"", UsageChannelExclude, 101},
		{"m1", UsageChannelAll, 11},
		{"m1", UsageChannelExclude, 1},
	} {
		filter := usageFilter(time.UTC, "2026-11-20", "2026-11-20")
		filter.Model, filter.Channel = tc.model, tc.channel
		activity, err := store.UsageActivity(context.Background(), filter)
		if err != nil {
			t.Fatal(err)
		}
		if activity.Range.Total != tc.want || activity.AllTime.Total != tc.want {
			t.Fatalf("%s/%s: range %d all %d, want %d", tc.model, tc.channel, activity.Range.Total, activity.AllTime.Total, tc.want)
		}
	}
	page, err := store.UsageSessions(context.Background(), usageFilter(time.UTC, "2026-11-20", "2026-11-20"), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ChannelTurns != 1 || page.Sessions[0].Turns != 3 {
		t.Fatalf("sessions %+v", page.Sessions)
	}
}

func TestUsageStreaksAndPeak(t *testing.T) {
	store := newUsageStore(t)
	for index, day := range []int{10, 11, 12, 15, 16, 17, 18, 19} {
		recordUsage(t, store, TurnUsageRecord{SessionID: "s", TurnID: string(rune('a' + index)), Model: "m", CompletedAt: time.Date(2026, 11, day, 12, 0, 0, 0, time.UTC), Usage: protocol.Usage{InputTokens: int(day)}})
	}
	filter := usageFilter(time.UTC, "2026-11-01", "2026-11-30")
	filter.Now = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	activity, err := store.UsageActivity(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	if activity.CurrentStreakDays != 5 || activity.LongestStreakDays != 5 {
		t.Fatalf("streaks current=%d longest=%d, want 5/5 (today without usage keeps the streak)", activity.CurrentStreakDays, activity.LongestStreakDays)
	}
	if activity.PeakDay == nil || activity.PeakDay.Date != "2026-11-19" {
		t.Fatalf("peak %+v", activity.PeakDay)
	}
	filter.Now = time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	activity, err = store.UsageActivity(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	if activity.CurrentStreakDays != 0 {
		t.Fatalf("current streak after a missed day = %d", activity.CurrentStreakDays)
	}
}

func TestUsageSessionsPaginatesByTotal(t *testing.T) {
	store := newUsageStore(t)
	at := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	for _, entry := range []struct {
		id    string
		input int
	}{{"a", 5}, {"b", 9}, {"c", 5}, {"d", 1}, {"e", 9}} {
		recordUsage(t, store, TurnUsageRecord{SessionID: entry.id, TurnID: "t", Model: "m", CompletedAt: at, Usage: protocol.Usage{InputTokens: entry.input}})
	}
	filter := usageFilter(time.UTC, "2026-11-20", "2026-11-20")
	var order []string
	cursor := ""
	for {
		page, err := store.UsageSessions(context.Background(), filter, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Sessions {
			order = append(order, entry.SessionID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if got := len(order); got != 5 || order[0] != "b" || order[1] != "e" || order[2] != "a" || order[3] != "c" || order[4] != "d" {
		t.Fatalf("order %v", order)
	}
	if _, err := store.UsageSessions(context.Background(), filter, "not-a-cursor!", 2); !errors.Is(err, ErrInvalidUsageCursor) {
		t.Fatalf("invalid cursor error = %v", err)
	}
}

func TestIsChannelRequest(t *testing.T) {
	if IsChannelRequest(nil) || IsChannelRequest(&protocol.RequestContext{Source: "completion_trigger"}) {
		t.Fatal("non-channel request classified as channel")
	}
	if !IsChannelRequest(&protocol.RequestContext{Source: "feishu", ConnectionID: "c1"}) {
		t.Fatal("channel request not classified")
	}
}
