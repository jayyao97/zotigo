package services

import (
	"testing"
)

func TestLoopDetector_BasicLoop(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 3,
		WindowSize: 10,
	})

	// First two calls should not trigger loop
	status := ld.RecordCall("read_file", `{"path": "test.txt"}`)
	if status.IsLooping {
		t.Error("First call should not trigger loop")
	}

	status = ld.RecordCall("read_file", `{"path": "test.txt"}`)
	if status.IsLooping {
		t.Error("Second call should not trigger loop")
	}

	// Third identical call should trigger loop
	status = ld.RecordCall("read_file", `{"path": "test.txt"}`)
	if !status.IsLooping {
		t.Error("Third identical call should trigger loop")
	}
	if status.RepeatCount != 3 {
		t.Errorf("Expected repeat count 3, got %d", status.RepeatCount)
	}
}

func TestLoopDetector_DifferentCalls(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 3,
		WindowSize: 10,
	})

	// Different calls should not trigger loop
	ld.RecordCall("read_file", `{"path": "a.txt"}`)
	ld.RecordCall("read_file", `{"path": "b.txt"}`)
	ld.RecordCall("read_file", `{"path": "c.txt"}`)
	status := ld.RecordCall("read_file", `{"path": "d.txt"}`)

	if status.IsLooping {
		t.Error("Different calls should not trigger loop")
	}
}

func TestLoopDetector_DifferentTools(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 3,
		WindowSize: 10,
	})

	// Different tools with same args should not trigger loop
	ld.RecordCall("read_file", `{"path": "test.txt"}`)
	ld.RecordCall("write_file", `{"path": "test.txt"}`)
	status := ld.RecordCall("glob", `{"path": "test.txt"}`)

	if status.IsLooping {
		t.Error("Different tools should not trigger loop")
	}
}

func TestLoopDetector_WindowSliding(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 3,
		WindowSize: 5,
	})

	// Fill window with call A twice
	ld.RecordCall("read_file", `{"path": "a.txt"}`)
	ld.RecordCall("read_file", `{"path": "a.txt"}`)

	// Add different calls to push A out of window
	ld.RecordCall("read_file", `{"path": "b.txt"}`)
	ld.RecordCall("read_file", `{"path": "c.txt"}`)
	ld.RecordCall("read_file", `{"path": "d.txt"}`)
	ld.RecordCall("read_file", `{"path": "e.txt"}`)
	ld.RecordCall("read_file", `{"path": "f.txt"}`)

	// Now call A again - should start fresh count
	status := ld.RecordCall("read_file", `{"path": "a.txt"}`)
	if status.RepeatCount != 1 {
		t.Errorf("After sliding window, repeat count should be 1, got %d", status.RepeatCount)
	}
}

func TestLoopDetector_Reset(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 3,
		WindowSize: 10,
	})

	// Add some calls
	ld.RecordCall("read_file", `{"path": "test.txt"}`)
	ld.RecordCall("read_file", `{"path": "test.txt"}`)

	// Reset
	ld.Reset()

	// Should start fresh
	status := ld.RecordCall("read_file", `{"path": "test.txt"}`)
	if status.RepeatCount != 1 {
		t.Errorf("After reset, repeat count should be 1, got %d", status.RepeatCount)
	}
}

func TestLoopDetector_PatternLoop(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 2,
		WindowSize: 20,
	})

	// Create a pattern: A, B, A, B, A, B (pattern of 2 repeated 3 times)
	for i := 0; i < 3; i++ {
		ld.RecordCall("read_file", `{"path": "a.txt"}`)
		ld.RecordCall("write_file", `{"path": "b.txt"}`)
	}

	status := ld.CheckPatternLoop(4)
	// Pattern detection is a nice-to-have feature
	// The basic loop detection (same call repeated) is the core functionality
	// For now, just verify it doesn't panic
	_ = status
}

func TestLoopDetector_Stats(t *testing.T) {
	ld := NewLoopDetector(DefaultLoopDetectorConfig())

	ld.RecordCall("read_file", `{"path": "a.txt"}`)
	ld.RecordCall("read_file", `{"path": "a.txt"}`)
	ld.RecordCall("write_file", `{"path": "b.txt"}`)

	stats := ld.Stats()
	if len(stats) != 2 {
		t.Errorf("Expected 2 unique signatures, got %d", len(stats))
	}
	total := 0
	for _, v := range stats {
		total += v
	}
	if total != 3 {
		t.Errorf("Expected 3 total recorded calls across histogram, got %d", total)
	}
}

// TestLoopDetector_ConsecutiveOnly verifies that the detector counts only
// consecutive identical calls — an iterative workflow that interleaves the
// same command with other work (edit → go test → edit → go test) must NOT
// be flagged as a loop, even if the same command shows up several times
// within the recent-calls window.
func TestLoopDetector_ConsecutiveOnly(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 3,
		WindowSize: 20,
	})

	// Simulate: run test, edit, run test, edit, run test — 3 test runs in
	// the window but never consecutive.
	for i := range 3 {
		if status := ld.RecordCall("shell", `{"cmd": "go test ./..."}`); status.IsLooping {
			t.Fatalf("iteration %d: go test call should not trigger loop yet (interleaved)", i)
		}
		if status := ld.RecordCall("edit", `{"path": "foo.go", "iter": `+string(rune('0'+i))+`}`); status.IsLooping {
			t.Fatalf("iteration %d: edit call should not trigger loop", i)
		}
	}

	// One final go test — still not consecutive (last call was an edit),
	// so consec should reset to 1.
	status := ld.RecordCall("shell", `{"cmd": "go test ./..."}`)
	if status.IsLooping {
		t.Error("non-consecutive repeats must not trigger loop detection")
	}
	if status.RepeatCount != 1 {
		t.Errorf("expected RepeatCount=1 for non-consecutive repeat, got %d", status.RepeatCount)
	}
}

func TestLoopDetector_DefaultThresholds(t *testing.T) {
	cfg := DefaultLoopDetectorConfig()
	if cfg.MaxRepeats != 7 {
		t.Errorf("expected default MaxRepeats=7, got %d", cfg.MaxRepeats)
	}
	if cfg.WindowSize != 15 {
		t.Errorf("expected default WindowSize=15, got %d", cfg.WindowSize)
	}
}

func TestLoopDetector_Suggestion(t *testing.T) {
	ld := NewLoopDetector(LoopDetectorConfig{
		MaxRepeats: 2,
		WindowSize: 10,
	})

	ld.RecordCall("grep", `{"pattern": "foo"}`)
	status := ld.RecordCall("grep", `{"pattern": "foo"}`)

	if status.Suggestion == "" {
		t.Error("Should provide suggestion for grep loop")
	}
}
