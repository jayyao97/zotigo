package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

type retryAttemptKey struct{}
type retryAttempt struct {
	status int
	header http.Header
}

// retryProvider only retries before publishing model output. Once output or a
// tool call has escaped, replay would need an explicit stream replacement protocol.
type retryProvider struct{ Provider }

// WithRetries owns the single retry budget for native adapters. Their SDK retry
// loops are disabled so one request cannot multiply into nested retry loops.
func WithRetries(factory FactoryFunc) FactoryFunc {
	return func(cfg config.ProfileConfig) (Provider, error) {
		provider, err := factory(cfg)
		if err != nil {
			return nil, err
		}
		return &retryProvider{Provider: provider}, nil
	}
}

func (p *retryProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolList []tools.Tool, opts ...StreamChatOption) (<-chan protocol.Event, error) {
	out := make(chan protocol.Event)
	go func() {
		defer close(out)
		requestID := uuid.NewString()
		retryCount := 0
		recovered := false
		report := func(status string, delay time.Duration) {
			out <- protocol.Event{Type: protocol.EventTypeRequestRetry, Retry: &protocol.RequestRetry{RequestID: requestID, Status: status, Attempt: retryCount, DelayMS: delay.Milliseconds()}}
		}
		fail := func(err error) {
			if retryCount == 0 || recovered {
				return
			}
			status := "failed"
			if ctx.Err() != nil {
				status = "canceled"
			}
			report(status, 0)
		}
		for attempt := 0; ; attempt++ {
			if err := ctx.Err(); err != nil {
				fail(err)
				out <- protocol.NewErrorEvent(err)
				return
			}
			state := &retryAttempt{}
			callCtx, cancel := context.WithCancel(context.WithValue(ctx, retryAttemptKey{}, state))
			stream, err := p.Provider.StreamChat(callCtx, messages, toolList, opts...)
			emitted := false
			terminal := protocol.Event{}
			if err == nil {
				for event := range stream {
					if event.Type == protocol.EventTypeError && event.Error != nil {
						terminal, err = event, event.Error
						cancel()
						for range stream {
						}
						break
					}
					if retryCount > 0 && !recovered {
						report("recovered", 0)
						recovered = true
					}
					emitted = true
					out <- event
				}
			}
			cancel()
			if err == nil {
				if retryCount > 0 && !recovered {
					report("recovered", 0)
				}
				return
			}
			if emitted || terminal.Usage != nil || attempt >= 2 || ctx.Err() != nil || !transientError(err, state) {
				if terminal.Error == nil {
					terminal = protocol.NewErrorEvent(err)
				}
				fail(err)
				out <- terminal
				return
			}
			delay := retryDelay(state.header, attempt)
			// Honor long Retry-After values by stopping rather than retrying early
			// or leaving an otherwise unbounded task asleep for hours.
			if delay > time.Minute {
				fail(err)
				out <- protocol.NewErrorEvent(err)
				return
			}
			retryCount = attempt + 1
			report("waiting", delay)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				fail(ctx.Err())
				out <- protocol.NewErrorEvent(ctx.Err())
				return
			case <-timer.C:
			}
			report("retrying", 0)
		}
	}()
	return out, nil
}

func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500 && status <= 599
}

func transientError(err error, state *retryAttempt) bool {
	// The caller's deadline is checked in StreamChat. Transport timeouts can
	// also wrap DeadlineExceeded while that caller remains alive.
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrContextLengthExceeded) || errors.Is(err, ErrRequestTooLarge) {
		return false
	}
	if state.header.Get("x-should-retry") == "false" {
		return false
	}
	if state.status >= 400 {
		return transientStatus(state.status)
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// The SDKs sometimes expose SSE errors as JSON embedded in an error string.
	// Inspect structured code/type only, never retry based on arbitrary prose.
	message := err.Error()
	if start := strings.IndexByte(message, '{'); start >= 0 {
		type streamError struct {
			Code json.RawMessage `json:"code"`
			Type string          `json:"type"`
		}
		var body struct {
			Error *streamError `json:"error"`
			streamError
		}
		if json.Unmarshal([]byte(message[start:]), &body) == nil {
			detail := body.Error
			if detail == nil {
				detail = &body.streamError
			}
			var status int
			if json.Unmarshal(detail.Code, &status) == nil && transientStatus(status) {
				return true
			}
			var code string
			_ = json.Unmarshal(detail.Code, &code)
			return transientCode(code) || transientCode(detail.Type)
		}
	}
	var coded interface{ RetryCode() string }
	return errors.As(err, &coded) && transientCode(coded.RetryCode())
}

func transientCode(code string) bool {
	switch code {
	case "server_error", "internal_error", "api_error", "overloaded_error", "rate_limit_error", "rate_limit_exceeded":
		return true
	}
	return false
}

func retryDelay(header http.Header, attempt int) time.Duration {
	delay := time.Second * time.Duration(1<<attempt)
	delay += time.Duration(rand.Int64N(int64(delay / 2)))
	if seconds, err := strconv.ParseInt(header.Get("Retry-After"), 10, 32); err == nil && seconds > 0 {
		return max(delay, time.Duration(seconds)*time.Second)
	}
	if date, err := http.ParseTime(header.Get("Retry-After")); err == nil {
		return max(delay, time.Until(date))
	}
	return delay
}
