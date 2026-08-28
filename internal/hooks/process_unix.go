//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

type processGroup struct{}

func newProcessGroup(command *exec.Cmd) (*processGroup, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processGroup{}, nil
}

func (*processGroup) attach(*exec.Cmd) error { return nil }

func (*processGroup) resume(*exec.Cmd) error { return nil }

func (*processGroup) terminate(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}

func (*processGroup) close() error { return nil }
