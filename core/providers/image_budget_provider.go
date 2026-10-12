package providers

import (
	"context"
	"errors"
	"fmt"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

// ErrRequestTooLarge is deliberately distinct from token/context exhaustion.
// Retrying compaction with the same oversized payload cannot recover a 413.
var ErrRequestTooLarge = errors.New("provider: request body too large")

type imageBudgetProvider struct {
	Provider
	limit  int
	images imageCache
}

// WithImageBudget is installed by native HTTP adapters alongside their guarded
// transport. Registry-only/custom providers retain their own stream contract.
func WithImageBudget(factory FactoryFunc) FactoryFunc {
	return func(cfg config.ProfileConfig) (Provider, error) {
		limit, err := requestByteLimit(cfg.Params)
		if err != nil {
			return nil, err
		}
		provider, err := factory(cfg)
		if err != nil {
			return nil, err
		}
		return &imageBudgetProvider{Provider: provider, limit: limit}, nil
	}
}

func (p *imageBudgetProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolList []tools.Tool, opts ...StreamChatOption) (<-chan protocol.Event, error) {
	prepared, omitted, err := p.images.prepareMessages(ctx, messages, 0, p.limit/2)
	if err != nil {
		return nil, err
	}
	out := make(chan protocol.Event)
	go func() {
		defer close(out)
		// Like the native streams, callers drain through close. Preserve terminal
		// events even after cancellation so usage and final state are not lost.
		send := func(event protocol.Event) { out <- event }
		limit := p.limit
		for attempt := 0; attempt < 3; attempt++ {
			if err := ctx.Err(); err != nil {
				return
			}
			debug.Logf("provider image budget: attempt=%d limit=%d omitted=%d", attempt, limit, omitted)
			budget := &requestBudget{limit: limit}
			attemptCtx, cancel := context.WithCancel(withRequestBudget(ctx, budget))
			stream, callErr := p.Provider.StreamChat(attemptCtx, prepared, toolList, opts...)
			emitted := false
			if callErr == nil {
				for event := range stream {
					if event.Type == protocol.EventTypeError && event.Error != nil {
						callErr = event.Error
						cancel()
						// Native adapters close their stream on error. Drain so none
						// can be left blocked sending during retry/cancellation.
						for range stream {
						}
						break
					}
					emitted = true
					send(event)
				}
			}
			cancel()
			debug.Logf("provider request budget: attempt=%d bytes=%d rejected=%v", attempt, budget.wireBytes.Load(), budget.tooLarge.Load())
			if callErr == nil {
				return
			}
			if !budget.tooLarge.Load() {
				send(protocol.NewErrorEvent(callErr))
				return
			}
			// Never replay a partially delivered stream or replay tool execution.
			if emitted || attempt == 2 || ctx.Err() != nil {
				send(protocol.NewErrorEvent(fmt.Errorf("%w: image reduction could not fit the request; reduce current input/attachments or start a new session (original history is unchanged)", ErrRequestTooLarge)))
				return
			}
			// Enforce strictly smaller serialized requests on the next attempt,
			// including when the upstream limit is smaller than our default.
			// Local rejection records limit+1: retain the configured hard cap
			// while reducing image quality. Halving the whole cap would also
			// remove room for text that image reduction cannot shrink.
			limit = min(limit, int(budget.wireBytes.Load())-1)
			if limit <= 0 {
				send(protocol.NewErrorEvent(ErrRequestTooLarge))
				return
			}
			prepared, omitted, err = p.images.prepareMessages(ctx, messages, attempt+1, limit/2)
			if err != nil {
				send(protocol.NewErrorEvent(err))
				return
			}
		}
	}()
	return out, nil
}
