package feishu

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type countingReadCloser struct {
	reader io.Reader
	read   int
}

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.read += count
	return count, err
}

func (*countingReadCloser) Close() error { return nil }

func TestResourceLimitTransportStopsOversizedStreamingBody(t *testing.T) {
	body := &countingReadCloser{reader: strings.NewReader(strings.Repeat("x", maxInboundImageBytes+1024))}
	transport := &resourceLimitTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, ContentLength: -1, Header: make(http.Header)}, nil
	})}
	request, err := http.NewRequest(http.MethodGet, "https://open.feishu.cn/open-apis/im/v1/messages/om_1/resources/img_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	if !errors.Is(err, errInboundImageTooLarge) {
		t.Fatalf("err=%v", err)
	}
	if body.read != maxInboundImageBytes+1 {
		t.Fatalf("read=%d want=%d", body.read, maxInboundImageBytes+1)
	}
}

func TestResourceLimitTransportRejectsKnownOversizedBodyBeforeReading(t *testing.T) {
	body := &countingReadCloser{reader: strings.NewReader("unused")}
	transport := &resourceLimitTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, ContentLength: maxInboundImageBytes + 1, Header: make(http.Header)}, nil
	})}
	request, _ := http.NewRequest(http.MethodGet, "https://open.feishu.cn/open-apis/im/v1/messages/om_1/resources/img_1", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, errInboundImageTooLarge) {
		t.Fatalf("err=%v", err)
	}
	if body.read != 0 {
		t.Fatalf("read=%d", body.read)
	}
}
