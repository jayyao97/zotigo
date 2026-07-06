//go:build !windows

package session

import (
	"os"
	"syscall"
)

func pidRunning(pid int) (bool, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	return process.Signal(syscall.Signal(0)) == nil, nil
}
