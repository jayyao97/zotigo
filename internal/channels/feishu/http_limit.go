package feishu

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errInboundImageTooLarge = errors.New("message image exceeds the size limit")

type resourceLimitTransport struct {
	base http.RoundTripper
}

func (t *resourceLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil || !strings.Contains(request.URL.Path, "/im/v1/messages/") || !strings.Contains(request.URL.Path, "/resources/") {
		return response, err
	}
	if response.ContentLength > maxInboundImageBytes {
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: %d bytes", errInboundImageTooLarge, response.ContentLength)
	}
	response.Body = &maxBytesReadCloser{source: response.Body, remaining: maxInboundImageBytes}
	return response, nil
}

type maxBytesReadCloser struct {
	source    io.ReadCloser
	remaining int64
}

func (r *maxBytesReadCloser) Read(buffer []byte) (int, error) {
	if r.remaining > 0 {
		if int64(len(buffer)) > r.remaining {
			buffer = buffer[:r.remaining]
		}
		count, err := r.source.Read(buffer)
		r.remaining -= int64(count)
		return count, err
	}
	var probe [1]byte
	count, err := r.source.Read(probe[:])
	if count > 0 {
		return 0, errInboundImageTooLarge
	}
	return 0, err
}

func (r *maxBytesReadCloser) Close() error {
	return r.source.Close()
}
