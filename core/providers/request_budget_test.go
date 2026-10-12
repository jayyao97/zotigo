package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWireBudgetRejectsBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if string(data) != "1234" {
			t.Errorf("request changed: %q", data)
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()
	client := RequestBudgetHTTPClient()
	for _, body := range []string{"12345", "1234"} {
		budget := &requestBudget{limit: 4}
		req, _ := http.NewRequestWithContext(withRequestBudget(context.Background(), budget), http.MethodPost, server.URL, strings.NewReader(body))
		req.ContentLength = -1 // Also enforce the bound for chunked SDK bodies.
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 413 || !budget.tooLarge.Load() {
			t.Fatal("413 not recognized")
		}
		if budget.wireBytes.Load() != int64(len(body)) {
			t.Fatal("wrong byte count")
		}
	}
	if calls != 1 {
		t.Fatalf("network calls=%d, want only the in-budget request", calls)
	}
}

func TestRequestBudgetConfiguration(t *testing.T) {
	for _, value := range []any{-1, 0, 1.5, "unlimited", 65 << 20} {
		if _, err := requestByteLimit(map[string]any{"max_request_bytes": value}); err == nil {
			t.Fatalf("accepted %v", value)
		}
	}
	if got, err := requestByteLimit(map[string]any{"max_request_bytes": float64(8 << 20)}); err != nil || got != 8<<20 {
		t.Fatalf("got %d %v", got, err)
	}
}
