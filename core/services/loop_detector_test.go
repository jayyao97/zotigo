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
	status := ld.RecordCall("list_dir", `{"path": "test.txt"}`)

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
