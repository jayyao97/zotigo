package providers

import (
	"context"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

// Provider defines the interface that all LLM providers must implement.
type Provider interface {
	// StreamChat sends the conversation history and available tools to the
	// model. Optional per-call knobs (tool choice, reasoning effort, ...)
	// travel through opts — see options.go for the available WithX
	// helpers. Passing no options gives the provider's default behavior.
	StreamChat(
		ctx context.Context,
		messages []protocol.Message,
		tools []tools.Tool,
		opts ...StreamChatOption,
	) (<-chan protocol.Event, error)

	// Name returns the provider's name (e.g., "openai", "claude").
	Name() string
}
