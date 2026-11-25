package zotigod

import (
	"context"
	"testing"
)

func TestChannelModelResolvesCodexChoices(t *testing.T) {
	h := &handler{runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, &fakeCodexRuntime{})}
	for _, test := range []struct {
		choice  []string
		current string
		model   string
		effort  string
	}{
		// Model names match without regard to case; an unsupported current effort falls back.
		{[]string{"GPT-5.6-luna"}, "high", "gpt-5.6-luna", "medium"},
		{[]string{"gpt-5.6-luna", "Medium"}, "", "gpt-5.6-luna", "medium"},
	} {
		model, effort, err := h.resolveCodexChoice(context.Background(), test.choice, test.current)
		if err != nil || model != test.model || effort != test.effort {
			t.Fatalf("%v: model=%q effort=%q err=%v", test.choice, model, effort, err)
		}
	}
	for _, choice := range [][]string{nil, {"gpt-5.6-luna", "high"}, {"unknown"}, {"a", "b", "c"}} {
		if _, _, err := h.resolveCodexChoice(context.Background(), choice, ""); err == nil {
			t.Fatalf("accepted %v", choice)
		}
	}
}
