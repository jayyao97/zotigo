package diagnostics

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
)

const monitorArgument = "--internal-crash-monitor"

// MonitorCrashes uses a separate process so runtime panic output can be drained
// after the crashing process has exited. The monitor shares normal log retention.
// Call the returned function on normal exit only, not while unwinding a panic.
func MonitorCrashes(component string) func() {
	if len(os.Args) == 2 && os.Args[1] == monitorArgument {
		// Drain after service-manager termination; EOF exits normally, while the
		// manager's final SIGKILL timeout remains a backstop for a stuck monitor.
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
		home, err := os.UserHomeDir()
		if err != nil {
			os.Exit(1)
		}
		output, err := newWriter(filepath.Join(home, ".zotigo", "logs", component), maxBytes, maxFiles, maxTotalBytes)
		if err != nil {
			os.Exit(1)
		}
		_, _ = os.Stdout.Write([]byte{1})
		_, err = io.Copy(output, os.Stdin)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		return func() {}
	}
	executable, err := os.Executable()
	if err != nil {
		return func() {}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	child := exec.Command(executable, monitorArgument)
	isolateMonitor(child)
	child.Stdin = reader
	child.Stderr = os.Stderr
	ready, err := child.StdoutPipe()
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return func() {}
	}
	if err = child.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		fmt.Fprintf(os.Stderr, "Crash logging unavailable: %v\n", err)
		return func() {}
	}
	_ = reader.Close()
	var acknowledgement [1]byte
	_, err = io.ReadFull(ready, acknowledgement[:])
	_ = ready.Close()
	if err != nil {
		_ = writer.Close()
		_ = child.Wait()
		fmt.Fprintf(os.Stderr, "Crash logging unavailable: %v\n", err)
		return func() {}
	}
	if err := debug.SetCrashOutput(writer, debug.CrashOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "Crash logging unavailable: %v\n", err)
	}
	_ = writer.Close() // The runtime retains its own descriptor until normal exit.
	return func() {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		_ = child.Wait()
	}
}
