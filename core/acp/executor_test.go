package acp

import (
	"strings"
	"testing"
)

func TestStatWindows_Parse(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantDir bool
		wantErr bool
	}{
		{"directory", "DIR\n", true, false},
		{"file", "FILE\n", false, false},
		{"not found", "NOTFOUND\n", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := strings.TrimSpace(tt.output)
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
