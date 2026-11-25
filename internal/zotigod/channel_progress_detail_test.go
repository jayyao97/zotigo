package zotigod

import (
	"context"
	"slices"
	"strings"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"
)

func progressDetailTurn() []zotigosession.DisplayItem {
	turn := &zotigosession.DisplayTurn{ID: "turn-1"}
	text := func(value string) zotigosession.DisplayContentPart {
		return zotigosession.DisplayContentPart{Type: "text", Text: value}
	}
	call := func(id, name, arguments string) zotigosession.DisplayContentPart {
		return zotigosession.DisplayContentPart{Type: "tool_call", ToolCall: &zotigosession.DisplayToolCall{ID: id, Name: name, Arguments: arguments}}
	}
	result := func(id, name string) zotigosession.DisplayContentPart {
		return zotigosession.DisplayContentPart{Type: "tool_result", ToolResult: &zotigosession.DisplayToolResult{ToolCallID: id, ToolName: name, Text: "private output"}}
	}
	started := func(sequence uint64, id, name string) zotigosession.DisplayItem {
		return zotigosession.DisplayItem{ID: "start-" + id, Sequence: sequence, Type: zotigosession.DisplayItemToolExecutionStarted, ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-1", ToolCallID: id, ToolName: name}}
	}
	assistant := func(sequence uint64, parts ...zotigosession.DisplayContentPart) zotigosession.DisplayItem {
		return zotigosession.DisplayItem{ID: "assistant-" + string(rune('a'+sequence)), Sequence: sequence, Type: zotigosession.DisplayItemAssistantMessage, Turn: turn, Content: parts}
	}
	return []zotigosession.DisplayItem{
		{ID: "turn", Sequence: 2, Type: zotigosession.DisplayItemTurnStarted, Turn: turn},
		assistant(3, zotigosession.DisplayContentPart{Type: "reasoning", Text: "plan A"}, text("Let me check the tests."), call("c1", "shell", `{"command":["bash","-lc","go   test ./..."]}`)),
		started(4, "c1", "shell"),
		assistant(5, result("c1", "shell")),
		assistant(6, text("Tests pass; now reading main.")),
		assistant(7, call("c2", "read_file", `{"path":"/repo/main.go"}`)),
		started(8, "c2", "read_file"),
		assistant(9, result("c2", "read_file")),
		assistant(10, text("All good.")),
		{ID: "done", Sequence: 11, Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1", LastAgentMessage: "All good."}},
	}
}

type projectedRun struct {
	subjects  []string
	narration []string
	thinking  []string
	details   []string
}

func runProjection(t *testing.T, source *sessionlogtest.ItemSource, detail string, projected *uint64, turnID *string) (projectedRun, bool) {
	t.Helper()
	var run projectedRun
	approval := false
	_, done, err := (&handler{items: source}).channelTaskResult(context.Background(), "session-1", 1, projected, turnID, &approval, channels.RuntimeAttribution{}, detail, func(progress channels.Progress) {
		run.details = append(run.details, progress.Detail)
		for _, event := range progress.Events {
			switch event.Type {
			case channels.ExecutionToolStarted:
				run.subjects = append(run.subjects, event.Tool.Subject)
			case channels.ExecutionNarration:
				run.narration = append(run.narration, event.Text)
			case channels.ExecutionThinking:
				run.thinking = append(run.thinking, event.Text)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return run, done
}

func TestChannelProgressDetailLevels(t *testing.T) {
	for _, test := range []struct {
		detail    string
		subjects  []string
		narration []string
		thinking  []string
	}{
		{channels.ProgressDetailBrief, []string{"", ""}, nil, nil},
		{channels.ProgressDetailDetailed, []string{"go test ./...", "/repo/main.go"}, []string{"Let me check the tests.", "Tests pass; now reading main."}, nil},
		{channels.ProgressDetailFull, []string{"go test ./...", "/repo/main.go"}, []string{"Let me check the tests.", "Tests pass; now reading main."}, []string{"plan A"}},
	} {
		source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{"session-1": progressDetailTurn()}}
		projected, turnID := uint64(1), ""
		run, done := runProjection(t, source, test.detail, &projected, &turnID)
		if !done {
			t.Fatalf("%s: turn not done", test.detail)
		}
		if !slices.Equal(run.subjects, test.subjects) || !slices.Equal(run.narration, test.narration) || !slices.Equal(run.thinking, test.thinking) {
			t.Fatalf("%s: %+v", test.detail, run)
		}
		for _, detail := range run.details {
			if detail != test.detail {
				t.Fatalf("%s: progress detail %q", test.detail, detail)
			}
		}
		// The final answer is delivered separately, and tool output never is.
		if strings.Contains(strings.Join(run.narration, "|"), "All good.") {
			t.Fatalf("%s: final answer was projected as narration", test.detail)
		}
	}
}

func TestChannelProgressHoldsTextUntilItProvesInterim(t *testing.T) {
	all := progressDetailTurn()
	// The first pass ends right after a text-only message.
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{"session-1": all[:5]}}
	projected, turnID := uint64(1), ""
	first, done := runProjection(t, source, channels.ProgressDetailDetailed, &projected, &turnID)
	if done || !slices.Equal(first.narration, []string{"Let me check the tests."}) {
		t.Fatalf("first pass done=%v %+v", done, first)
	}
	if projected >= 6 {
		t.Fatalf("held text was marked projected: %d", projected)
	}
	source.Items["session-1"] = all
	second, done := runProjection(t, source, channels.ProgressDetailDetailed, &projected, &turnID)
	if !done || !slices.Equal(second.narration, []string{"Tests pass; now reading main."}) || !slices.Equal(second.subjects, []string{"/repo/main.go"}) {
		t.Fatalf("second pass done=%v %+v", done, second)
	}
}

func TestToolSubjectSummarizesArguments(t *testing.T) {
	for arguments, want := range map[string]string{
		`{"command":"ls\n  -la"}`:                        "ls -la",
		`{"cmd":["bash","-lc","make build"]}`:            "make build",
		`{"pattern":"TODO","path":"src"}`:                "src",
		`{"query":"feishu cot"}`:                         "feishu cot",
		`{"content":"secret"}`:                           "",
		`not json`:                                       "",
		`{"command":"` + strings.Repeat("x", 100) + `"}`: strings.Repeat("x", 80) + "…",
	} {
		if got := toolSubject(arguments); got != want {
			t.Fatalf("toolSubject(%s) = %q, want %q", arguments, got, want)
		}
	}
}

func TestChannelProgressKeepsFinalThinkingButNotTheAnswer(t *testing.T) {
	items := progressDetailTurn()
	final := &items[len(items)-2]
	final.Content = append([]zotigosession.DisplayContentPart{{Type: "thinking", Text: "wrap up"}}, final.Content...)
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{"session-1": items[:len(items)-1]}}
	projected, turnID := uint64(1), ""
	// While the turn runs, the final message is held together with its thinking.
	running, done := runProjection(t, source, channels.ProgressDetailFull, &projected, &turnID)
	if done || slices.Contains(running.thinking, "wrap up") {
		t.Fatalf("running pass done=%v %+v", done, running)
	}
	source.Items["session-1"] = items
	finished, done := runProjection(t, source, channels.ProgressDetailFull, &projected, &turnID)
	if !done || !slices.Equal(finished.thinking, []string{"wrap up"}) || slices.Contains(finished.narration, "All good.") {
		t.Fatalf("finished pass done=%v %+v", done, finished)
	}
}

func TestToolSubjectRedactsCredentials(t *testing.T) {
	for arguments, want := range map[string]string{
		`{"command":"curl -H 'Authorization: Bearer abc123' https://x"}`: "curl -H 'Authorization: Bearer ***' https://x",
		`{"url":"https://api.example.com/v1?api_key=SECRET&page=2"}`:     "https://api.example.com/v1?api_key=***&page=2",
		`{"command":"GITHUB_TOKEN=ghp_xyz make release"}`:                "GITHUB_TOKEN=*** make release",
		`{"command":"mysql --password=hunter2 db"}`:                      "mysql --password=*** db",
		`{"command":"go test ./internal/tokenizer/..."}`:                 "go test ./internal/tokenizer/...",
	} {
		if got := toolSubject(arguments); got != want {
			t.Fatalf("toolSubject(%s) = %q, want %q", arguments, got, want)
		}
	}
}
