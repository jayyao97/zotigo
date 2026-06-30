package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m *Model) inputLineCount() int {
	val := m.input.Value()
	if val == "" {
		return 1
	}

	w := m.input.Width()
	if w < 1 {
		w = 1
	}

	lines := 0
	lastLineRemainder := 0
	for _, line := range strings.Split(val, "\n") {
		if line == "" {
			lines++
			lastLineRemainder = w
			continue
		}
		lineWidth := lipgloss.Width(line)
		visualLines := (lineWidth + w - 1) / w
		if visualLines < 1 {
			visualLines = 1
		}
		lines += visualLines
		lastLineRemainder = w - (lineWidth % w)
		if lastLineRemainder == w {
			lastLineRemainder = 0
		}
	}

	if lines < 1 {
		lines = 1
	}
	// Reserve one more line when the current visual line is full.
	if lastLineRemainder == 0 {
		lines++
	}
	return lines
}

func isImagePath(s string) bool {
	ext := strings.ToLower(filepath.Ext(s))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return true
	}
	return false
}

func (m *Model) handlePaste(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)

	if !strings.Contains(trimmed, "\n") && isImagePath(trimmed) {
		if newPath, err := m.storeImage(trimmed); err == nil {
			return fmt.Sprintf("@%s", newPath), true
		}
	}

	return "", false
}

func (m *Model) storeImage(srcPath string) (string, error) {
	// Save to current directory's .zotigo folder for shorter paths
	uploadDir := ".zotigo/uploads"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return "", err
	}

	ext := filepath.Ext(srcPath)
	filename := fmt.Sprintf("img_%d%s", time.Now().UnixNano(), ext)
	destPath := filepath.Join(uploadDir, filename)

	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}

	return destPath, nil
}

func isSpecialKey(k tea.KeyPressMsg) bool {
	// Keys that should always be forwarded to textarea even without Text:
	// navigation, deletion, modifiers, function keys, etc.
	s := k.String()
	switch {
	case strings.HasPrefix(s, "ctrl+"),
		strings.HasPrefix(s, "alt+"),
		strings.HasPrefix(s, "shift+"):
		return true
	}
	switch k.Code {
	case tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight,
		tea.KeyHome, tea.KeyEnd, tea.KeyPgUp, tea.KeyPgDown,
		tea.KeyDelete, tea.KeyBackspace, tea.KeyTab,
		tea.KeyEnter, tea.KeyEscape:
		return true
	}
	return false
}

func (m *Model) pasteImageFromClipboard() (string, bool) {
	// Only support Mac for now via osascript
	if runtime.GOOS != "darwin" {
		return "", false
	}

	// Save to current directory's .zotigo folder for shorter paths
	uploadDir := ".zotigo/uploads"
	_ = os.MkdirAll(uploadDir, 0755)

	filename := fmt.Sprintf("paste_%d.png", time.Now().UnixNano())
	relPath := filepath.Join(uploadDir, filename)

	// AppleScript needs absolute path
	absPath, err := filepath.Abs(relPath)
	if err != nil {
		return "", false
	}

	// AppleScript to save clipboard to file
	script := fmt.Sprintf(`try
		set theFile to (open for access POSIX file "%s" with write permission)
		set eof theFile to 0
		write (the clipboard as «class PNGf») to theFile
		close access theFile
		return "OK"
	on error
		try
			close access theFile
		end try
		return "ERR"
	end try`, absPath)

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.Output()

	if err == nil && strings.TrimSpace(string(out)) == "OK" {
		info, err := os.Stat(relPath)
		if err == nil && info.Size() > 0 {
			return relPath, true // Return relative path for display
		}
	}
	_ = os.Remove(relPath)

	return "", false
}
