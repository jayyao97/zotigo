package zotigod

import (
	"io"
	"os/exec"
	"slices"
	"testing"
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
	if err := launcher.startTracked("session-1", "test worker", exec.Command("sleep", "30")); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		launcher.Close()
		close(closed)
	}()
	<-closed
	if err := launcher.startTracked("session-2", "late worker", exec.Command("sleep", "30")); err == nil {
		t.Fatal("late worker start succeeded after launcher shutdown")
	}
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.commands) != 0 {
		t.Fatalf("tracked commands after close = %d", len(launcher.commands))
	}
}
