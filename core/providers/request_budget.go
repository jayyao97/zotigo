package providers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

const defaultRequestBytes = 24 << 20

func requestByteLimit(params map[string]any) (int, error) {
	value, ok := params["max_request_bytes"]
	if !ok {
		return defaultRequestBytes, nil
	}
	limit, err := strconv.ParseFloat(fmt.Sprint(value), 64)
	if err != nil || math.IsNaN(limit) || limit != math.Trunc(limit) || limit < 64<<10 || limit > 64<<20 {
		return 0, fmt.Errorf("max_request_bytes must be an integer between 65536 and 67108864")
	}
	return int(limit), nil
}

type requestBudgetKey struct{}

// Per-attempt state connects the SDK's actual wire request to its owning call.
// Do not infer a 413 from error text (which may contain untrusted response text).
type requestBudget struct {
	limit     int
	tooLarge  atomic.Bool
	wireBytes atomic.Int64
}

func withRequestBudget(ctx context.Context, budget *requestBudget) context.Context {
	return context.WithValue(ctx, requestBudgetKey{}, budget)
}

type requestBudgetTransport struct{ next http.RoundTripper }

// RequestBudgetHTTPClient enforces the budget after SDK serialization, including
// system prompts, tool schemas and base64 expansion. Unrelated SDK calls pass through.
func RequestBudgetHTTPClient() *http.Client {
	return &http.Client{Transport: &requestBudgetTransport{next: http.DefaultTransport}}
}

func (t *requestBudgetTransport) RoundTrip(req *http.Request) (response *http.Response, err error) {
	defer func() {
		if attempt, ok := req.Context().Value(retryAttemptKey{}).(*retryAttempt); ok && response != nil {
			attempt.status = response.StatusCode
			attempt.header = response.Header.Clone()
		}
	}()
	budget, ok := req.Context().Value(requestBudgetKey{}).(*requestBudget)
	if !ok || req.Body == nil || req.Method != http.MethodPost {
		return t.next.RoundTrip(req)
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, int64(budget.limit)+1))
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	budget.wireBytes.Store(int64(len(data)))
	if len(data) > budget.limit {
		budget.tooLarge.Store(true)
		// A real non-retryable HTTP status avoids SDK transport-error retries.
		return &http.Response{
			StatusCode: http.StatusRequestEntityTooLarge,
			Status:     "413 Request Entity Too Large",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"request_too_large","message":"Request exceeds local byte budget"}}`)),
			Request:    req,
		}, nil
	}
	request := req.Clone(req.Context())
	request.Body = io.NopCloser(bytes.NewReader(data))
	response, err = t.next.RoundTrip(request)
	if response != nil && response.StatusCode == http.StatusRequestEntityTooLarge {
		budget.tooLarge.Store(true)
	}
	return response, err
}
