//go:build linux || darwin

package zotigodclient

import (
	"os/exec"
	"syscall"
)

func detachProcess(cmd *exec.Cmd) {
	// A new session keeps the daemon alive when the terminal that started it
	// closes and keeps terminal signals (Ctrl+C) away from it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
