package zotigod

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/session"
)

func TestSessionToolsConversationProjectionAndContinuation(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	appendItem := func(item session.DisplayItem) session.DisplayItem {
		t.Helper()
		got, err := h.store.AppendDisplayItem(ctx, caller.SessionID, item)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	image := appendItem(session.DisplayItem{Type: session.DisplayItemUserMessage, Content: []session.DisplayContentPart{{Type: "image", Image: &session.DisplayMediaPart{FileID: "photo", MediaType: "image/png", Data: []byte("PRIVATE_IMAGE_BYTES"), URL: "PRIVATE_IMAGE_URL"}}}})
	tool := appendItem(session.DisplayItem{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "tool_result", ToolResult: &session.DisplayToolResult{Text: "PRIVATE_TOOL_BODY"}}}})
	text := strings.Repeat("中文🙂\x01", 20000)
	mixed := appendItem(session.DisplayItem{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "tool_call", ToolCall: &session.DisplayToolCall{Arguments: "PRIVATE_TOOL_ARGS"}}, {Type: "text", Text: text}}})
	appendItem(session.DisplayItem{Type: session.DisplayItemTurnCompleted})
	read := func(args map[string]any) map[string]any {
		t.Helper()
		args["session_id"] = caller.SessionID
		raw, _ := json.Marshal(args)
		got, err := h.readToolSession(ctx, caller, raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(got)
		if err != nil || len(encoded) > 64*1024 || strings.Contains(string(encoded), "PRIVATE_") {
			t.Fatalf("unsafe history projection: size=%d err=%v", len(encoded), err)
		}
		return got.(map[string]any)
	}
	first := read(map[string]any{"after": image.Sequence - 1, "limit": 100})
	rows := first["items"].([]toolHistoryItem)
	if len(rows) != 1 || rows[0].Sequence != image.Sequence || len(rows[0].Attachments) != 1 || rows[0].Attachments[0].FileID != "photo" || first["next_cursor"] != strconv.FormatUint(image.Sequence, 10) {
		t.Fatalf("image or cursor lost: %+v", first)
	}
	second := read(map[string]any{"after": image.Sequence, "limit": 100})
	rows = second["items"].([]toolHistoryItem)
	if len(rows) != 1 || rows[0].Sequence != mixed.Sequence || !rows[0].Truncated || rows[0].NextTextOffset == nil || second["next_cursor"] != "" {
		t.Fatalf("long mixed message: %+v", second)
	}
	row := rows[0]
	full := row.Text
	for row.NextTextOffset != nil {
		offset := *row.NextTextOffset
		if offset != len(full) {
			t.Fatalf("offset=%d consumed=%d", offset, len(full))
		}
		next := read(map[string]any{"message_sequence": mixed.Sequence, "text_offset": offset})
		if next["prev_cursor"] != "" || next["next_cursor"] != "" {
			t.Fatal("single-message read returned surrounding history cursors")
		}
		rows = next["items"].([]toolHistoryItem)
		row = rows[0]
		if row.Text == "" || !utf8.ValidString(row.Text) {
			t.Fatal("continuation made no UTF-8 progress")
		}
		full += row.Text
	}
	if full != text {
		t.Fatalf("message not fully recovered: got=%d want=%d", len(full), len(text))
	}
	for _, args := range []map[string]any{
		{"message_sequence": tool.Sequence},
		{"message_sequence": mixed.Sequence, "text_offset": 1},
		{"message_sequence": mixed.Sequence, "text_offset": len(text) + 1},
		{"message_sequence": mixed.Sequence, "limit": 1},
		{"message_sequence": mixed.Sequence, "before": mixed.Sequence},
		{"text_offset": 1},
		{"text_offset": -1},
		{"message_sequence": mixed.Sequence, "until": "2000-01-01T00:00:00Z"},
	} {
		args["session_id"] = caller.SessionID
		raw, _ := json.Marshal(args)
		if _, err := h.readToolSession(ctx, caller, raw); err == nil {
			t.Fatalf("invalid continuation accepted: %s", raw)
		}
	}
}

func TestHistoryBudgetPreservesBothPaginationDirections(t *testing.T) {
	items := make([]session.DisplayItem, 9)
	for i := range items {
		items[i] = session.DisplayItem{Sequence: uint64(i + 1), Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "text", Text: strings.Repeat("x", 12000)}}}
	}
	for _, forward := range []bool{false, true} {
		query := session.DisplayPageQuery{Limit: 100, HasAfter: forward}
		var seen []uint64
		for round := 0; round < 10; round++ {
			page := session.PageDisplayItems(items, query)
			rows, err := projectToolHistory(&page, forward, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				t.Fatal("empty intermediate page")
			}
			var ids []uint64
			for _, row := range rows {
				if len(row.Text) != 12000 || row.Truncated {
					t.Fatal("whole message unnecessarily truncated")
				}
				ids = append(ids, row.Sequence)
			}
			cursor := page.NextCursor
			if forward {
				seen = append(seen, ids...)
			} else {
				seen = append(ids, seen...)
				cursor = page.PrevCursor
			}
			if cursor == "" {
				break
			}
			sequence, err := strconv.ParseUint(cursor, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if forward {
				query.After = sequence
			} else {
				query.HasBefore = true
				query.Before = sequence
			}
		}
		if !reflect.DeepEqual(seen, []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9}) {
			t.Fatalf("forward=%v skipped/repeated messages: %v", forward, seen)
		}
	}
}

func TestSessionToolsOptionalToolDetailsAndReasoningFilter(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := t.Context()
	text := strings.Repeat("工具结果🙂\n", 20000)
	items := []session.DisplayItem{
		{Type: session.DisplayItemUserMessage, Content: []session.DisplayContentPart{{Type: "text", Text: "question"}}},
		{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "reasoning", Text: "PRIVATE_REASONING"}}},
		{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{
			{Type: "tool_call", ToolCall: &session.DisplayToolCall{ID: "call", Name: "shell", Arguments: `{"command":"go test ./..."}`}},
			{Type: "tool_result", ToolResult: &session.DisplayToolResult{ToolCallID: "call", ToolName: "shell", IsError: true, Text: text, Content: []session.DisplayToolResultContentPart{{Type: "image", Image: &session.DisplayMediaPart{FileID: "img", Data: []byte("PRIVATE_IMAGE"), URL: "PRIVATE_URL"}}}}},
			{Type: "thinking", Text: "PRIVATE_THINKING"},
			{Type: "text", Text: "done"},
		}},
		{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "tool_result", ToolResult: &session.DisplayToolResult{Text: "standalone result"}}}},
	}
	for i := range items {
		item, err := h.store.AppendDisplayItem(ctx, caller.SessionID, items[i])
		if err != nil {
			t.Fatal(err)
		}
		items[i] = item
	}
	read := func(args map[string]any) map[string]any {
		t.Helper()
		args["session_id"] = caller.SessionID
		raw, _ := json.Marshal(args)
		result, err := h.readToolSession(ctx, caller, raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "PRIVATE_") {
			t.Fatal("private reasoning/media leaked")
		}
		rows := result.(map[string]any)["items"].([]toolHistoryItem)
		encodedRows, _ := json.Marshal(rows)
		if len(encodedRows) > toolHistoryPageBytes+2 {
			t.Fatalf("budget exceeded: %d", len(encodedRows))
		}
		return result.(map[string]any)
	}
	plain := read(map[string]any{"after": items[0].Sequence - 1, "limit": 2})["items"].([]toolHistoryItem)
	if len(plain) != 2 || plain[1].Sequence != items[2].Sequence || plain[1].Text != "done" {
		t.Fatalf("default projection = %+v", plain)
	}
	if result := read(map[string]any{"after": items[2].Sequence, "include_tools": true}); len(result["items"].([]toolHistoryItem)) != 1 {
		t.Fatal("tool-only result omitted")
	}
	row := read(map[string]any{"message_sequence": items[2].Sequence, "include_tools": true})["items"].([]toolHistoryItem)[0]
	if !row.Truncated || row.NextTextOffset == nil {
		t.Fatal("long tool result not resumable")
	}
	full := row.Text
	for row.NextTextOffset != nil {
		row = read(map[string]any{"message_sequence": items[2].Sequence, "include_tools": true, "text_offset": *row.NextTextOffset})["items"].([]toolHistoryItem)[0]
		if row.Text == "" {
			t.Fatal("no continuation progress")
		}
		full += row.Text
	}
	// Decode the result from the reconstructed labeled stream, ensuring chunks
	// preserved the exact output rather than only its length or prefix.
	start := strings.Index(full, "[tool_result]\n") + len("[tool_result]\n")
	end := strings.Index(full[start:], "\ndone")
	var part session.DisplayContentPart
	if start < len("[tool_result]\n") || end < 0 {
		t.Fatal("tool result labels missing")
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(full[start:start+end])), &part); err != nil {
		t.Fatal(err)
	}
	if part.ToolResult.Text != text || !part.ToolResult.IsError || part.ToolResult.ToolCallID != "call" {
		t.Fatal("tool result changed")
	}
	if !strings.Contains(full, "go test ./...") {
		t.Fatal("call arguments missing")
	}
	for _, limit := range []int{-1, 101} {
		raw, _ := json.Marshal(map[string]any{"session_id": caller.SessionID, "limit": limit, "include_tools": true})
		if _, err := h.readToolSession(ctx, caller, raw); err == nil {
			t.Fatalf("invalid limit %d accepted", limit)
		}
	}
}
