package zotigodclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// After a dropped connection the stream resumes from the last delivered
// durable sequence and never delivers an item twice.
func TestSubscribeReconnectsFromCursorWithoutDuplicates(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		item := func(seq int) {
			_, _ = fmt.Fprintf(w, "id: %d\nevent: item\ndata: {\"id\":\"i%d\",\"sequence\":%d,\"type\":\"user_message\"}\n\n", seq, seq, seq)
		}
		switch connections.Add(1) {
		case 1:
			if r.URL.Query().Get("after") != "" {
				t.Errorf("first connection after = %q", r.URL.Query().Get("after"))
			}
			item(1)
			_, _ = fmt.Fprint(w, "event: delta\ndata: {\"item_id\":\"a\",\"part_type\":\"text\",\"delta\":\"x\"}\n\n")
			item(2)
			flusher.Flush()
			// Drop the connection mid-stream.
		default:
			if got := r.URL.Query().Get("after"); got != "2" {
				t.Errorf("reconnect after = %q, want 2", got)
			}
			// A server replaying an overlapping window must not duplicate.
			item(2)
			item(3)
			flusher.Flush()
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events := New(server.URL, "").Subscribe(ctx, "sess", 0, StreamOptions{})
	var sequences []uint64
	reconnects, deltas := 0, 0
	for len(sequences) < 3 {
		select {
		case event := <-events:
			switch {
			case event.Item != nil:
				sequences = append(sequences, event.Item.Sequence)
			case event.Delta != nil:
				deltas++
			case event.Reconnecting:
				reconnects++
			case event.Err != nil:
				t.Fatalf("stream ended: %v", event.Err)
			}
		case <-ctx.Done():
			t.Fatalf("timed out, got %v", sequences)
		}
	}
	if fmt.Sprint(sequences) != "[1 2 3]" || reconnects != 1 || deltas != 1 {
		t.Fatalf("sequences=%v reconnects=%d deltas=%d", sequences, reconnects, deltas)
	}
}

func TestSubscribeStopsOnClientError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"session not found"}`))
	}))
	defer server.Close()
	events := New(server.URL, "").Subscribe(context.Background(), "missing", 0, StreamOptions{})
	event := <-events
	if !IsCode(event.Err, "not_found") || event.Reconnecting {
		t.Fatalf("event = %+v", event)
	}
	if _, open := <-events; open {
		t.Fatal("stream should close after a client error")
	}
}
