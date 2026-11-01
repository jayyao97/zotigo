//go:build !linux && !darwin

package zotigodclient

import "os/exec"

func detachProcess(cmd *exec.Cmd) {}
