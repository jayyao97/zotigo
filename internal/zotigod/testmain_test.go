package zotigod

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "zotigod-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create isolated test home:", err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, "set isolated test home:", err)
		_ = os.RemoveAll(home)
		os.Exit(1)
	}

	code := m.Run()
	if err := os.RemoveAll(home); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "remove isolated test home:", err)
		code = 1
	}
	os.Exit(code)
}
