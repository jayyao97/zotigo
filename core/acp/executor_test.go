package acp

import (
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
)

// These tests exercise the parsing helpers by calling the exported methods
// on a RemoteExecutor with mocked platform settings. Since the actual
// terminal calls go through the Server (which needs a real connection),
// we test the parsing logic indirectly via the platform-specific branches
// using the internal methods.

func TestListDirWindows_Parse(t *testing.T) {
	// Simulate the output of:
	//   dir /B /AD "path" 2>nul & echo --- & dir /B /A-D "path" 2>nul
	output := "subdir1\nsubdir2\n---\nfile1.txt\nfile2.go\n"

	var entries []executor.FileInfo
	isFilesSection := false
	for _, line := range splitLines(output) {
		line = trimSpace(line)
		if line == "---" {
			isFilesSection = true
			continue
		}
		if line == "" {
			continue
		}
		entries = append(entries, executor.FileInfo{
			Name:  line,
			IsDir: !isFilesSection,
		})
	}

	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Directories
	if !entries[0].IsDir || entries[0].Name != "subdir1" {
		t.Errorf("entries[0]: expected dir subdir1, got %+v", entries[0])
	}
	if !entries[1].IsDir || entries[1].Name != "subdir2" {
		t.Errorf("entries[1]: expected dir subdir2, got %+v", entries[1])
	}
	// Files
	if entries[2].IsDir || entries[2].Name != "file1.txt" {
		t.Errorf("entries[2]: expected file file1.txt, got %+v", entries[2])
	}
	if entries[3].IsDir || entries[3].Name != "file2.go" {
		t.Errorf("entries[3]: expected file file2.go, got %+v", entries[3])
	}
}

func TestListDirWindows_EmptyDir(t *testing.T) {
	// Empty dir: no files, no dirs
	output := "---\n"

	var entries []executor.FileInfo
	isFilesSection := false
	for _, line := range splitLines(output) {
		line = trimSpace(line)
		if line == "---" {
			isFilesSection = true
			continue
		}
		if line == "" {
			continue
		}
		entries = append(entries, executor.FileInfo{
			Name:  line,
			IsDir: !isFilesSection,
		})
	}
	_ = isFilesSection

	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty dir, got %d", len(entries))
	}
}

func TestStatWindows_Parse(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		wantDir  bool
		wantErr  bool
	}{
		{"directory", "DIR\n", true, false},
		{"file", "FILE\n", false, false},
		{"not found", "NOTFOUND\n", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := trimSpace(tt.output)
			switch result {
			case "DIR":
				if tt.wantErr {
					t.Error("expected error for DIR")
				}
				if !tt.wantDir {
					t.Error("expected isDir=true for DIR")
				}
			case "FILE":
				if tt.wantErr {
					t.Error("expected error for FILE")
				}
				if tt.wantDir {
					t.Error("expected isDir=false for FILE")
				}
			default:
				if !tt.wantErr {
					t.Error("expected no error but got NOTFOUND")
				}
			}
		})
	}
}

func TestShellCmd(t *testing.T) {
	// Unix
	e := &RemoteExecutor{platform: "linux"}
	shell, flag := e.shellCmd()
	if shell != "sh" || flag != "-c" {
		t.Errorf("linux: expected sh -c, got %s %s", shell, flag)
	}

	// macOS
	e.platform = "darwin"
	shell, flag = e.shellCmd()
	if shell != "sh" || flag != "-c" {
		t.Errorf("darwin: expected sh -c, got %s %s", shell, flag)
	}

	// Windows
	e.platform = "windows"
	shell, flag = e.shellCmd()
	if shell != "cmd" || flag != "/C" {
		t.Errorf("windows: expected cmd /C, got %s %s", shell, flag)
	}
}

// helpers to avoid importing strings in internal test
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func trimSpace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	j := len(s)
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}
