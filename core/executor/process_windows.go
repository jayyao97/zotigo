//go:build windows

package executor

import (
	"os"
	"os/exec"
	"strconv"
)

func prepareCommandForCancellation(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid)).Run(); err == nil {
			return nil
		}
		return command.Process.Kill()
	}
}
