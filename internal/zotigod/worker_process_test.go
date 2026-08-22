package zotigod

import (
	"io"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestWorkerProcessEnvReplacesInheritedAuthToken(t *testing.T) {
	got := workerProcessEnv([]string{
		"PATH=/bin",
		workerAuthTokenEnv + "=stale-secret",
	}, "current-secret")
	want := []string{"PATH=/bin", workerAuthTokenEnv + "=current-secret"}
	if !slices.Equal(got, want) {
		t.Fatalf("workerProcessEnv = %#v, want %#v", got, want)
	}
}

func TestProcessWorkerLauncherCloseKillsTrackedProcessAndRejectsLateStart(t *testing.T) {
	launcher := &processWorkerLauncher{commands: make(map[string]*exec.Cmd), output: io.Discard}
	if err := launcher.startTracked("session-1", "test worker", exec.Command("sleep", "30"), nil); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		launcher.Close()
		close(closed)
	}()
	<-closed
	if err := launcher.startTracked("session-2", "late worker", exec.Command("sleep", "30"), nil); err == nil {
		t.Fatal("late worker start succeeded after launcher shutdown")
	}
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.commands) != 0 {
		t.Fatalf("tracked commands after close = %d", len(launcher.commands))
	}
}

func TestProcessWorkerLauncherCallsExitHook(t *testing.T) {
	launcher := &processWorkerLauncher{commands: make(map[string]*exec.Cmd), output: io.Discard}
	exited := make(chan struct{})
	if err := launcher.startTracked("session-1", "test worker", exec.Command("true"), func() { close(exited) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("worker exit hook was not called")
	}
	launcher.Close()
}

func TestProcessWorkerLauncherReplacementAndCloseCallEachExitHook(t *testing.T) {
	launcher := &processWorkerLauncher{commands: make(map[string]*exec.Cmd), output: io.Discard}
	firstExited := make(chan struct{})
	secondExited := make(chan struct{})
	otherExited := make(chan struct{})
	if err := launcher.startTracked("session-1", "first", exec.Command("sleep", "30"), func() { close(firstExited) }); err != nil {
		t.Fatal(err)
	}
	if err := launcher.startTracked("session-1", "replacement", exec.Command("sleep", "30"), func() { close(secondExited) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstExited:
	case <-time.After(time.Second):
		t.Fatal("replaced worker exit hook was not called")
	}
	select {
	case <-secondExited:
		t.Fatal("replacement worker exited before launcher close")
	default:
	}
	if err := launcher.startTracked("session-2", "other", exec.Command("sleep", "30"), func() { close(otherExited) }); err != nil {
		t.Fatal(err)
	}
	launcher.Close()
	for name, exited := range map[string]<-chan struct{}{"replacement": secondExited, "other": otherExited} {
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Fatalf("%s worker exit hook was not called", name)
		}
	}
}
