//go:build !linux && !darwin

package diagnostics

import "os/exec"

func isolateMonitor(command *exec.Cmd) {}
