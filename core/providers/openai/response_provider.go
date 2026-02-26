package openai

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/openai/openai-go/v3"
)

type ResponseProvider struct {
	client *openai.Client
	model  string
}

func (p *ResponseProvider) Name() string {
	return "openai-response"
}

func (p *ResponseProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolsList []tools.Tool) (<-chan protocol.Event, error) {
	return nil, fmt.Errorf("responses api implementation pending SDK verification")
}
