package zotigod

import (
	"bytes"
	"testing"

	"github.com/jayyao97/zotigo/core/workspace"
)

func TestCatalogMigrateCommand(t *testing.T) {
	root := t.TempDir()
	s, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := runCatalogMigrate([]string{"--root", root, "--to", "7"}, &output, &output); code != 1 {
		t.Fatalf("active catalog exit = %d", code)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"7", "8"} {
		if code := runCatalogMigrate([]string{"--root", root, "--to", version}, &output, &output); code != 0 {
			t.Fatalf("migration exit = %d: %s", code, output.String())
		}
	}
	for _, args := range [][]string{nil, {"--to", "7"}, {"--root", root}, {"--root", root, "--to", "8", "extra"}} {
		if code := runCatalogMigrate(args, &output, &output); code != 2 {
			t.Fatalf("invalid arguments %v exit = %d", args, code)
		}
	}
}
