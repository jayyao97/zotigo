package tools

import "testing"

func TestReadOnlyScope_InWorkDir(t *testing.T) {
	c := SafetyCall{
		Arguments: `{"path":"src/foo.go"}`,
		WorkDir:   "/proj",
		SafeDirs:  []string{"/proj"},
	}
	d := ReadOnlyScope("path")(c)
	if d.Level != LevelSafe {
		t.Errorf("expected Safe, got %s", d.Level)
	}
}

func TestReadOnlyScope_OutsideSafeScope(t *testing.T) {
	c := SafetyCall{
		Arguments: `{"path":"/etc/passwd"}`,
		WorkDir:   "/proj",
		SafeDirs:  []string{"/proj"},
	}
	d := ReadOnlyScope("path")(c)
	if d.Level != LevelMedium {
		t.Errorf("expected Medium, got %s", d.Level)
	}
}

func TestReadOnlyScope_SensitiveInsideWorkDir(t *testing.T) {
	c := SafetyCall{
		Arguments: `{"path":"/proj/.git/config"}`,
		WorkDir:   "/proj",
		SafeDirs:  []string{"/proj"},
	}
	d := ReadOnlyScope("path")(c)
	if d.Level != LevelMedium {
		t.Errorf("sensitive path should escalate even within workdir, got %s", d.Level)
	}
}

func TestMutatorScope_LowInWorkDir(t *testing.T) {
	c := SafetyCall{
		Arguments: `{"path":"main.go"}`,
		WorkDir:   "/proj",
	}
	d := MutatorScope("path")(c)
	if d.Level != LevelLow {
		t.Errorf("expected Low, got %s", d.Level)
	}
	if !d.RequiresSnapshot {
		t.Error("mutator scope must request snapshot")
	}
}

func TestMutatorScope_OutsideWorkDir(t *testing.T) {
	c := SafetyCall{
		Arguments: `{"path":"/tmp/elsewhere"}`,
		WorkDir:   "/proj",
	}
	d := MutatorScope("path")(c)
	if d.Level != LevelMedium {
		t.Errorf("expected Medium, got %s", d.Level)
	}
}

func TestMutatorScope_SensitiveInWorkDir(t *testing.T) {
	c := SafetyCall{
		Arguments: `{"path":"/proj/.env"}`,
		WorkDir:   "/proj",
	}
	d := MutatorScope("path")(c)
	if d.Level != LevelMedium {
		t.Errorf("sensitive path inside workdir should escalate, got %s", d.Level)
	}
}
