package zotigod

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestCompactingInputRejectionIsStructured(t *testing.T) {
	for _, endpoint := range []string{"messages", "steering"} {
		t.Run(endpoint, func(t *testing.T) {
			source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
			api := newHandler(newSessionRegistry(), source)
			server := httptest.NewServer(api)
			defer server.Close()
			created := createSession(t, api)
			startSession(t, api, created.ID)
			worker := dialWorker(t, server, created.ID)
			defer worker.Close()
			appendTurnStarted(t, source, created.ID, "compact")
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				api.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/"+endpoint, strings.NewReader(`{"text":"continue"}`)))
				done <- rec
			}()
			if err := worker.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var input wire.Message
			if err := worker.ReadJSON(&input); err != nil {
				t.Fatal(err)
			}
			if input.InputRequest == nil {
				t.Fatal("missing input request")
			}
			code, message := sessionlog.InputErrorDetails(fmt.Errorf("%w: context compaction is in progress", sessionlog.ErrActiveTurn))
			if err := worker.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &wire.InputResult{
				RequestID: input.InputRequest.RequestID, ErrorCode: code, Error: message,
			}}); err != nil {
				t.Fatal(err)
			}
			select {
			case rec := <-done:
				assertAPIError(t, rec, http.StatusConflict, "active_turn", "context compaction is in progress")
			case <-time.After(3 * time.Second):
				t.Fatal("rejection not returned")
			}
		})
	}
}
