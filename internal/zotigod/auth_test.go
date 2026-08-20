package zotigod

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthenticatedHandlerUsesSeparatePublicAndWorkerTokens(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := authenticatedHandler(next, "public-secret", "worker-secret")

	tests := []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{name: "health remains public", path: "/health", status: http.StatusNoContent},
		{name: "public missing", path: "/sessions", status: http.StatusUnauthorized},
		{name: "public wrong", path: "/sessions", token: "wrong", status: http.StatusUnauthorized},
		{name: "public accepted", path: "/sessions", token: "public-secret", status: http.StatusNoContent},
		{name: "public token rejected internally", path: "/internal/sessions/id/commands", token: "public-secret", status: http.StatusUnauthorized},
		{name: "worker accepted internally", path: "/internal/sessions/id/commands", token: "worker-secret", status: http.StatusNoContent},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, test.status, resp.Body.String())
			}
			if test.status == http.StatusUnauthorized && resp.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", resp.Header().Get("WWW-Authenticate"))
			}
			if test.status == http.StatusUnauthorized && !strings.Contains(resp.Body.String(), `"code":"unauthorized"`) {
				t.Fatalf("unauthorized response body = %s", resp.Body.String())
			}
		})
	}
}

func TestAuthenticatedHandlerAllowsLocalCompatibilityWithoutTokens(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := authenticatedHandler(next, "", "")

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNoContent)
	}
}

func TestAuthenticatedHandlerDoesNotExposeInternalRoutesWithOnlyPublicToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := authenticatedHandler(next, "public-secret", "")
	req := httptest.NewRequest(http.MethodGet, "/internal/sessions/id/commands", nil)
	req.Header.Set("Authorization", "Bearer public-secret")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
}

func TestLoadAuthToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zotigod.token")
	if err := os.WriteFile(path, []byte("  secret-value\n"), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	token, err := loadAuthToken(path)
	if err != nil {
		t.Fatalf("loadAuthToken: %v", err)
	}
	if token != "secret-value" {
		t.Fatalf("token = %q, want secret-value", token)
	}
}

func TestLoadAuthTokenRejectsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zotigod.token")
	if err := os.WriteFile(path, []byte(" \n"), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if _, err := loadAuthToken(path); err == nil {
		t.Fatal("loadAuthToken returned nil error for empty token")
	}
}

func TestListenAddressNeedsAuth(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{addr: "127.0.0.1:8765", want: false},
		{addr: "localhost:8765", want: false},
		{addr: "[::1]:8765", want: false},
		{addr: "10.20.30.40:8765", want: true},
		{addr: "0.0.0.0:8765", want: true},
		{addr: ":8765", want: true},
	}
	for _, test := range tests {
		t.Run(test.addr, func(t *testing.T) {
			if got := listenAddressNeedsAuth(test.addr); got != test.want {
				t.Fatalf("listenAddressNeedsAuth(%q) = %v, want %v", test.addr, got, test.want)
			}
		})
	}
}

func TestResolveWorkerDaemonURL(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		explicit string
		want     string
	}{
		{name: "loopback", addr: "127.0.0.1:8765", want: "http://127.0.0.1:8765"},
		{name: "ipv4 wildcard", addr: "0.0.0.0:8765", want: "http://127.0.0.1:8765"},
		{name: "empty wildcard", addr: ":8765", want: "http://127.0.0.1:8765"},
		{name: "ipv6 wildcard", addr: "[::]:8765", want: "http://[::1]:8765"},
		{name: "scoped ipv6", addr: "[fe80::1%en0]:8765", want: "http://[fe80::1%25en0]:8765"},
		{name: "explicit wins", addr: "0.0.0.0:8765", explicit: "http://10.20.30.40:9000/", want: "http://10.20.30.40:9000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveWorkerDaemonURL(test.addr, test.explicit)
			if err != nil {
				t.Fatalf("resolveWorkerDaemonURL: %v", err)
			}
			if got != test.want {
				t.Fatalf("url = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveWorkerDaemonURLRejectsEmptyQuery(t *testing.T) {
	if _, err := resolveWorkerDaemonURL("127.0.0.1:8765", "http://127.0.0.1:8765?"); err == nil {
		t.Fatal("resolveWorkerDaemonURL accepted an empty query")
	}
}

func TestWorkerHTTPClientSendsBearerToken(t *testing.T) {
	requestToken := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestToken = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newWorkerHTTPClient("worker-secret")
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if requestToken != "Bearer worker-secret" {
		t.Fatalf("Authorization = %q, want Bearer worker-secret", requestToken)
	}
}
