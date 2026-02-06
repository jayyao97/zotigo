package environment

import (
	"context"
	"testing"
)

func TestNewLocal(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := t.TempDir()

	env, err := NewLocal(tmpDir, dataDir)
	if err != nil {
		t.Fatalf("NewLocal failed: %v", err)
	}
	defer env.Close()

	// Check type
	if env.Type() != TypeLocal {
		t.Errorf("Expected type %s, got %s", TypeLocal, env.Type())
	}

	// Check executor
	if env.Executor() == nil {
		t.Error("Executor should not be nil")
	}

	// Check store
	if env.Store() == nil {
		t.Error("Store should not be nil")
	}

	// Test init
	ctx := context.Background()
	if err := env.Init(ctx); err != nil {
		t.Errorf("Init failed: %v", err)
	}
}

func TestNewLocal_DefaultDataDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Empty dataDir should use default (~/.zotigo)
	env, err := NewLocal(tmpDir, "")
	if err != nil {
		t.Fatalf("NewLocal failed: %v", err)
	}
	defer env.Close()

	if env.Store() == nil {
		t.Error("Store should not be nil even with default dataDir")
	}
}

func TestNewCustom(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := t.TempDir()

	// Create components manually
	localEnv, err := NewLocal(tmpDir, dataDir)
	if err != nil {
		t.Fatalf("NewLocal failed: %v", err)
	}
	defer localEnv.Close()

	// Create custom environment using those components
	customEnv := NewCustom(localEnv.Executor(), localEnv.Store())

	if customEnv.Type() != TypeCustom {
		t.Errorf("Expected type %s, got %s", TypeCustom, customEnv.Type())
	}

	if customEnv.Executor() != localEnv.Executor() {
		t.Error("Executor mismatch")
	}

	if customEnv.Store() != localEnv.Store() {
		t.Error("Store mismatch")
	}
}

func TestNewCustomWithType(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := t.TempDir()

	localEnv, err := NewLocal(tmpDir, dataDir)
	if err != nil {
		t.Fatalf("NewLocal failed: %v", err)
	}
	defer localEnv.Close()

	// Create custom environment with specific type
	customEnv := NewCustomWithType(localEnv.Executor(), localEnv.Store(), TypeE2B)

	if customEnv.Type() != TypeE2B {
		t.Errorf("Expected type %s, got %s", TypeE2B, customEnv.Type())
	}
}

func TestNew_Local(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := t.TempDir()

	env, err := New(Config{
		Type:    TypeLocal,
		WorkDir: tmpDir,
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer env.Close()

	if env.Type() != TypeLocal {
		t.Errorf("Expected type %s, got %s", TypeLocal, env.Type())
	}
}

func TestNew_Custom(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := t.TempDir()

	localEnv, err := NewLocal(tmpDir, dataDir)
	if err != nil {
		t.Fatalf("NewLocal failed: %v", err)
	}
	defer localEnv.Close()

	env, err := New(Config{
		Type: TypeCustom,
		Custom: &CustomConfig{
			Executor: localEnv.Executor(),
			Store:    localEnv.Store(),
		},
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	if env.Type() != TypeCustom {
		t.Errorf("Expected type %s, got %s", TypeCustom, env.Type())
	}
}

func TestNew_E2B_NotImplemented(t *testing.T) {
	_, err := New(Config{
		Type: TypeE2B,
		E2B:  &E2BConfig{},
	})
	if err == nil {
		t.Error("Expected error for unimplemented e2b environment")
	}
}

func TestNew_Docker_NotImplemented(t *testing.T) {
	_, err := New(Config{
		Type:   TypeDocker,
		Docker: &DockerConfig{},
	})
	if err == nil {
		t.Error("Expected error for unimplemented docker environment")
	}
}

func TestNew_InvalidType(t *testing.T) {
	_, err := New(Config{
		Type: "invalid",
	})
	if err == nil {
		t.Error("Expected error for invalid type")
	}
}

func TestNew_Custom_MissingConfig(t *testing.T) {
	_, err := New(Config{
		Type: TypeCustom,
		// Missing Custom config
	})
	if err == nil {
		t.Error("Expected error for missing custom config")
	}
}
