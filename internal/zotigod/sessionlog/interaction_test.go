package sessionlog

import (
	"reflect"
	"testing"

	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestInteractionDisplayRedactsSecretAnswers(t *testing.T) {
	req, err := NewUserInputInteraction("session", "turn", "item", wire.InteractionRequester{Agent: "codex"}, []wire.InteractionQuestion{
		{ID: "visible", Question: "Visible?"},
		{ID: "secret", Question: "Secret?", IsSecret: true},
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Answers = map[string][]string{"visible": {"yes"}, "secret": {"credential"}}
	item := InteractionItem(req, true)
	if got := item.Interaction.Answers["visible"]; !reflect.DeepEqual(got, []string{"yes"}) {
		t.Fatalf("visible answer = %#v", got)
	}
	if got := item.Interaction.Answers["secret"]; !reflect.DeepEqual(got, []string{"[redacted]"}) {
		t.Fatalf("secret answer was persisted: %#v", got)
	}
}

func TestInteractionAnswersRejectUnknownChoice(t *testing.T) {
	req, err := NewUserInputInteraction("session", "turn", "item", wire.InteractionRequester{}, []wire.InteractionQuestion{{
		ID: "choice", Question: "Choose", Options: []wire.InteractionOption{{Label: "A"}},
	}}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateInteractionAnswers(req, map[string][]string{"choice": {"B"}}); err == nil {
		t.Fatal("unknown choice was accepted")
	}
}

func TestInteractionAnswersAcceptExactlyOneOptionOrUserNote(t *testing.T) {
	req, err := NewUserInputInteraction("session", "turn", "item", wire.InteractionRequester{}, []wire.InteractionQuestion{{
		ID: "choice", Question: "Choose", IsOther: true, Options: []wire.InteractionOption{{Label: "A"}},
	}}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, answers := range []map[string][]string{
		{"choice": {"A"}},
		{"choice": {"A", "user_note: context"}},
		{"choice": {"user_note: custom answer"}},
		{"choice": {"None of the above", "user_note: custom answer"}},
		{"choice": {}},
	} {
		if err := ValidateInteractionAnswers(req, answers); err != nil {
			t.Fatalf("valid answers %#v rejected: %v", answers, err)
		}
	}
	for _, answers := range []map[string][]string{
		{"choice": {"unknown", "user_note: custom answer"}},
		{"choice": {"None of the above", "not a note"}},
		{"choice": {"user_note:   "}},
		{"choice": {"unknown"}},
	} {
		if err := ValidateInteractionAnswers(req, answers); err == nil {
			t.Fatalf("invalid answers %#v accepted", answers)
		}
	}
}
