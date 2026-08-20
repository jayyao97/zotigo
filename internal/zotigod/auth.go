package zotigod

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const workerAuthTokenEnv = "ZOTIGOD_WORKER_AUTH_TOKEN"

func authenticatedHandler(next http.Handler, publicToken string, workerToken string) http.Handler {
	if publicToken == "" && workerToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		internal := strings.HasPrefix(r.URL.Path, "/internal/")
		expected := publicToken
		if internal {
			expected = workerToken
		}
		if expected == "" && !internal {
			next.ServeHTTP(w, r)
			return
		}
		if expected != "" && bearerTokenMatches(r.Header.Get("Authorization"), expected) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
	})
}

func bearerTokenMatches(header string, expected string) bool {
	scheme, actual, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || actual == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func loadAuthToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read auth token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("auth token file is empty")
	}
	return token, nil
}

func generateAuthToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate worker auth token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func listenAddressNeedsAuth(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

func resolveWorkerDaemonURL(addr string, explicit string) (string, error) {
	if value := strings.TrimRight(strings.TrimSpace(explicit), "/"); value != "" {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("parse worker daemon url: %w", err)
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return "", fmt.Errorf("worker daemon url must use http or https")
		}
		if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return "", fmt.Errorf("worker daemon url must not contain credentials, path, query, or fragment")
		}
		return value, nil
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr, nil
	}
	ip := net.ParseIP(host)
	switch {
	case host == "" || (ip != nil && ip.IsUnspecified() && ip.To4() != nil):
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified():
		host = "::1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}).String(), nil
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func newWorkerHTTPClient(token string) *http.Client {
	client := &http.Client{Timeout: workerHTTPTimeout}
	if token != "" {
		client.Transport = bearerTransport{base: http.DefaultTransport, token: token}
	}
	return client
}
