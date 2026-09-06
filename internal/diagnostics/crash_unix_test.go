//go:build linux || darwin

package diagnostics

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMonitorDrainsAfterServiceTermination(t *testing.T) {
	home := t.TempDir()
	child := exec.Command(os.Args[0], monitorArgument)
	child.Env = append(os.Environ(), "HOME="+home)
	isolateMonitor(child)
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	ready, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = child.Process.Kill(); _ = child.Wait() })
	var acknowledgement [1]byte
	if _, err := io.ReadFull(ready, acknowledgement[:]); err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(child.Process.Pid)
	if err != nil || group != child.Process.Pid {
		t.Fatalf("monitor not isolated: group=%d error=%v", group, err)
	}
	// systemd sends termination to each cgroup member even in another group.
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(input, "supervisor-crash-marker\n"); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(home, ".zotigo/logs/daemon/run-*.log"))
	if len(files) != 1 {
		t.Fatalf("log files = %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "supervisor-crash-marker") {
		t.Fatalf("log = %q", data)
	}
}
