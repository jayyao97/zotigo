package worker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

// writeWorkerTestJSON answers a stubbed daemon endpoint the way the daemon's
// own JSON writer does: the bare value, no envelope, application/json.
func writeWorkerTestJSON(w http.ResponseWriter, status int, value any) {
	data, err := sonic.Marshal(value)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"encode response failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// workerUnavailableStore is a session store whose reads and writes fail with
// err. Only the methods the worker's profile persistence reaches are
// implemented; any other call would hit the nil embedded store and panic.
type workerUnavailableStore struct {
	zotigosession.Store
	err error
}

func (s workerUnavailableStore) Get(context.Context, string) (*zotigosession.Session, error) {
	return nil, s.err
}

func (s workerUnavailableStore) Put(context.Context, *zotigosession.Session) error {
	return s.err
}

type blockingFinishProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingFinishProvider) Name() string { return "blocking-finish" }

func (p *blockingFinishProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event, 2)
	go func() {
		defer close(events)
		close(p.started)
		<-p.release
		events <- protocol.NewTextDeltaEvent("old generation")
		events <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return events, nil
}

type blockingProfileUpdateStore struct {
	zotigosession.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingProfileUpdateStore) UpdateProfile(ctx context.Context, id string, profileName string, updatedAt time.Time) error {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return s.Store.(interface {
		UpdateProfile(context.Context, string, string, time.Time) error
	}).UpdateProfile(ctx, id, profileName, updatedAt)
}

func newTestStoreRoot(t *testing.T) string {
	t.Helper()
	return catalogtest.NewRoot(t)
}

type noopProvider struct{}

func (p *noopProvider) Name() string { return "noop" }

func (p *noopProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event)
	close(ch)
	return ch, nil
}

func tinyPNGBase64() string {
	return "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
}

type rollbackFailingProfileStore struct {
	zotigosession.Store
	updates int
}

func (s *rollbackFailingProfileStore) UpdateProfile(ctx context.Context, id string, profileName string, updatedAt time.Time) error {
	s.updates++
	if s.updates == 2 {
		return errors.New("rollback unavailable")
	}
	return s.Store.(interface {
		UpdateProfile(context.Context, string, string, time.Time) error
	}).UpdateProfile(ctx, id, profileName, updatedAt)
}

func writeDaemonTestSkill(t *testing.T, root, name, description, instructions string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + instructions + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
