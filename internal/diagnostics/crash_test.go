package diagnostics

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == monitorArgument {
		MonitorCrashes("daemon")
	}
	os.Exit(m.Run())
}

func TestRuntimeCrashIsPersisted(t *testing.T) {
	if os.Getenv("ZOTIGO_TEST_CRASH") == "1" {
		MonitorCrashes("daemon")
		go func() { panic("runtime-crash-marker") }()
		select {}
	}
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeCrashIsPersisted$")
	cmd.Env = append(os.Environ(), "HOME="+home, "ZOTIGO_TEST_CRASH=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("crashing subprocess unexpectedly succeeded")
	}
	// The monitor drains independently after the crashing process exits.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(home, ".zotigo/logs/daemon/run-*.log"))
		for _, file := range files {
			data, _ := os.ReadFile(file)
			if strings.Contains(string(data), "runtime-crash-marker") && strings.Contains(string(data), "crash_test.go") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runtime crash traceback was not persisted")
}
