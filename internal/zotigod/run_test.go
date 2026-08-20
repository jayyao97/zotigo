package zotigod

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestRunCreatesMissingConfigAndContinuesStartup(t *testing.T) {
	home := filepath.Join(t.TempDir(), "new-home")
	t.Setenv("HOME", home)
	output := captureStderr(t, func() {
		if code := Run([]string{"-addr", "invalid"}); code != 1 {
			t.Fatalf("Run exit code = %d, want listener failure code 1", code)
		}
	})

	path := filepath.Join(home, config.ConfigDirName, config.ConfigFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if !strings.Contains(output, path) || !strings.Contains(output, "API key") {
		t.Fatalf("startup output = %q", output)
	}
	if !strings.Contains(output, "Listening on http://invalid") || !strings.Contains(output, "Server failed") {
		t.Fatalf("zotigod did not continue to listener startup: %q", output)
	}
}

func TestRunRejectsNonLoopbackAddressWithoutAuthToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	output := captureStderr(t, func() {
		if code := Run([]string{"--addr", "0.0.0.0:8765"}); code != 1 {
			t.Fatalf("Run exit code = %d, want authentication failure code 1", code)
		}
	})
	if !strings.Contains(output, "non-loopback listen address requires --auth-token-file") {
		t.Fatalf("startup output = %q", output)
	}
}

func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous }()

	run()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}
	return string(output)
}
