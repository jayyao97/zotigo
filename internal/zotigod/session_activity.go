package zotigod

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"
)

type sessionActivitySnapshot struct {
	Revision uint64                  `json:"revision"`
	Active   map[string]SessionState `json:"active"`
}

func (r *sessionRegistry) watchActivity() (sessionActivitySnapshot, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sessionActivitySnapshot{r.activityRevision, maps.Clone(r.activity)}, r.activityChanged
}

// Each frame replaces the previous active set, including on reconnect.
// Capture the wakeup and snapshot together so changes during a write aren't lost.
func (h *handler) handleSessionActivity(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	heartbeat := time.NewTicker(displayEventHeartbeat)
	defer heartbeat.Stop()
	for {
		snapshot, changed := h.registry.watchActivity()
		data, err := json.Marshal(snapshot)
		if err != nil || setDisplayEventWriteDeadline(w) != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "event: activity\ndata: %s\n\n", data); err != nil {
			return
		}
		flusher.Flush()
		if clearDisplayEventWriteDeadline(w) != nil {
			return
		}
	wait:
		for {
			select {
			case <-req.Context().Done():
				return
			case <-changed:
				break wait
			case <-heartbeat.C:
				if writeDisplayEventComment(w, flusher, "keepalive") != nil {
					return
				}
			}
		}
	}
}
