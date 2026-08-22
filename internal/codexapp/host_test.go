package codexapp

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestHostCannotRestartAfterClose(t *testing.T) {
	host, err := NewHost("/usr/bin/false", t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Ensure after Close error = %v", err)
	}
}
