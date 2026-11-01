package zotigod

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/bytedance/sonic"
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

	// sonic compiles a decoder per type on first use. For workerMessage
	// that takes about 0.1s, and about 1s under -race, which otherwise lands
	// inside whichever test first reads a worker message and can exceed its
	// timing assertions (for example the 1s wait in
	// TestSessionMessageAllowsApprovalCallbackBeforeInputResult).
	if err := sonic.Pretouch(reflect.TypeOf(workerMessage{})); err != nil {
		fmt.Fprintln(os.Stderr, "pretouch worker message decoder:", err)
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
