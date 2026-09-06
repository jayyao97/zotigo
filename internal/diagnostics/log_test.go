package diagnostics

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func assertBudget(t *testing.T, dir string, limit int64, keep int) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "run-*.log"))
	if err != nil || len(files) == 0 || len(files) > keep {
		t.Fatalf("log files=%v, err=%v", files, err)
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.Size() > limit {
			t.Fatalf("log exceeds byte limit: %s, %v", file, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("log permissions=%o", info.Mode().Perm())
		}
	}
	return files
}

func TestRotationBoundsBytesAndRestartHistory(t *testing.T) {
	dir := t.TempDir()
	for range 10 {
		w, err := newWriter(dir, 128, 4, 512)
		if err != nil {
			t.Fatal(err)
		}
		p := bytes.Repeat([]byte("x"), 1000)
		if n, err := w.Write(p); n != len(p) || err != nil {
			t.Fatalf("write=%d, %v", n, err)
		}
	}
	for _, file := range assertBudget(t, dir, 128, 4) {
		data, _ := os.ReadFile(file)
		if !bytes.Contains(data, []byte("[truncated]")) {
			t.Fatalf("oversized record was not marked: %q", data)
		}
	}
}

func TestConcurrentWritersShareBudget(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 12 {
		w, err := newWriter(dir, 256, 128, 1024)
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			for range 20 {
				_, _ = w.Write(bytes.Repeat([]byte("a"), 100))
			}
		})
	}
	wg.Wait()
	assertBudget(t, dir, 256, 128)
	assertTotalBudget(t, dir, 1024)
}

func TestWriterRecoversWhenItsFileIsPruned(t *testing.T) {
	dir := t.TempDir()
	w, _ := newWriter(dir, 256, 1, 1024)
	_, _ = w.Write([]byte("old\n"))
	old := w.path
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("still-running\n"))
	files := assertBudget(t, dir, 256, 1)
	data, _ := os.ReadFile(files[0])
	if files[0] == old || !strings.Contains(string(data), "still-running") {
		t.Fatalf("writer did not reopen a retained file: %s %q", files[0], data)
	}
}

func TestWriteFailureDoesNotFailApplication(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	w, _ := newWriter(dir, 128, 4, 512)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("diagnostic failure escaped: n=%d err=%v", n, err)
	}
}

func assertTotalBudget(t *testing.T, dir string, budget int64) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "run-*.log"))
	var total int64
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total > budget {
		t.Fatalf("retained bytes = %d, budget = %d", total, budget)
	}
}

func TestSmallRestartLogsSurviveUntilBudgetOrCountLimit(t *testing.T) {
	dir := t.TempDir()
	for range 10 {
		w, _ := newWriter(dir, 128, 12, 512)
		_, _ = w.Write([]byte("small\n"))
	}
	if files := assertBudget(t, dir, 128, 12); len(files) != 10 {
		t.Fatalf("small logs prematurely pruned: %d", len(files))
	}
	for range 10 {
		w, _ := newWriter(dir, 128, 12, 512)
		_, _ = w.Write([]byte("small\n"))
	}
	if files := assertBudget(t, dir, 128, 12); len(files) != 12 {
		t.Fatalf("file count = %d", len(files))
	}
	assertTotalBudget(t, dir, 512)
}

func TestTotalBytesPruneOldestBeforeFileCountLimit(t *testing.T) {
	dir := t.TempDir()
	for i := range 10 {
		w, _ := newWriter(dir, 128, 128, 350)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 100))
		stamp := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(w.path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	files := assertBudget(t, dir, 128, 128)
	if len(files) != 3 {
		t.Fatalf("retained files = %d", len(files))
	}
	for _, file := range files {
		info, _ := os.Stat(file)
		if info.ModTime().Unix() < 8 {
			t.Fatal("oldest log retained")
		}
	}
	assertTotalBudget(t, dir, 350)
}

func TestSeparateProcessesDoNotMixRecords(t *testing.T) {
	if dir := os.Getenv("ZOTIGO_TEST_LOG_DIR"); dir != "" {
		w, err := newWriter(dir, 4096, 128, 1024*1024)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 25 {
			_, _ = fmt.Fprintf(w, "%s:%d\n", os.Getenv("ZOTIGO_TEST_LOG_ID"), i)
		}
		return
	}
	dir := t.TempDir()
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	for i := range 8 {
		command := exec.Command(os.Args[0], "-test.run=^TestSeparateProcessesDoNotMixRecords$")
		command.Env = append(os.Environ(), "ZOTIGO_TEST_LOG_DIR="+dir, fmt.Sprintf("ZOTIGO_TEST_LOG_ID=%d", i))
		output := new(bytes.Buffer)
		command.Stdout = output
		command.Stderr = output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
		outputs = append(outputs, output)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("child failed: %v %s", err, outputs[i])
		}
		if strings.Contains(outputs[i].String(), "logging failed") {
			t.Fatal(outputs[i].String())
		}
	}
	files := assertBudget(t, dir, 4096, 128)
	if len(files) != 8 {
		t.Fatalf("files=%d", len(files))
	}
	seen := make(map[string]bool)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 25 {
			t.Fatalf("record count=%d", len(lines))
		}
		id := strings.SplitN(lines[0], ":", 2)[0]
		if seen[id] {
			t.Fatalf("duplicate worker %s", id)
		}
		seen[id] = true
		for i, line := range lines {
			if line != fmt.Sprintf("%s:%d", id, i) {
				t.Fatalf("mixed/torn record: %q", line)
			}
		}
	}
}
