package deepseek

import (
	"context"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// Provider applies DeepSeek-specific request policy before delegating wire
// conversion and streaming to the Anthropic Messages provider.
type Provider struct {
	providers.Provider
}

func (p *Provider) Name() string { return ProviderName }

func (p *Provider) StreamChat(
	ctx context.Context,
	messages []protocol.Message,
	toolsList []tools.Tool,
	opts ...providers.StreamChatOption,
) (<-chan protocol.Event, error) {
	resolved := providers.ResolveOptions(opts)
	if resolved.ToolChoice.Mode != providers.ToolChoiceAuto {
		// DeepSeek thinking mode supports tool calls but rejects the
		// tool_choice parameter. Options are applied in order, so this final
		// auto value removes any required or named choice without discarding
		// unrelated options such as reasoning effort.
		opts = append(opts, providers.WithToolChoice(providers.ToolChoice{}))
	}
	return p.Provider.StreamChat(ctx, messages, toolsList, opts...)
}
