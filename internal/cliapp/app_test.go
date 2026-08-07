package cliapp

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestRunCreatesMissingConfigAndExits(t *testing.T) {
	home := filepath.Join(t.TempDir(), "new-home")
	t.Setenv("HOME", home)
	output := captureStdout(t, func() {
		if code := Run(nil); code != 0 {
			t.Fatalf("Run exit code = %d, want 0", code)
		}
	})

	path := filepath.Join(home, config.ConfigDirName, config.ConfigFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if !strings.Contains(output, path) || !strings.Contains(output, "Config created") || !strings.Contains(output, "API key") {
		t.Fatalf("startup output = %q", output)
	}
}

func captureStdout(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous }()

	run()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return string(output)
}
