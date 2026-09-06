//go:build linux || darwin

package diagnostics

import (
	"os/exec"
	"syscall"
)

func isolateMonitor(command *exec.Cmd) {
	// launchd kills remnants of the original job's process group on failure.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
