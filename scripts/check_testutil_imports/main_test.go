package main

import (
	"strings"
	"testing"
)

func TestForbiddenImports(t *testing.T) {
	for _, tc := range []struct {
		name, path, source string
		want               int
	}{
		{"direct", "core/example.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil"`, 1},
		{"alias", "core/example.go", `package p; import helper "github.com/jayyao97/zotigo/internal/testutil/catalogtest"`, 1},
		{"group", "core/example.go", "package p\nimport (\n . \"github.com/jayyao97/zotigo/internal/testutil/a\"\n _ \"github.com/jayyao97/zotigo/internal/testutil/b\"\n)", 2},
		{"raw", "core/example.go", "package p; import `github.com/jayyao97/zotigo/internal/testutil/a`", 1},
		{"escaped", "core/example.go", `package p; import "github.com/jayyao97/zotigo/internal/test\x75til/a"`, 1},
		{"other platform", "core/example_windows.go", "//go:build windows\n\npackage p\nimport \"github.com/jayyao97/zotigo/internal/testutil/a\"", 1},
		{"test", "core/example_test.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil/a"`, 0},
		{"helper", "internal/testutil/a/helper.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil/b"`, 0},
		{"similar directory", "internal/testutil_other/helper.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil/a"`, 1},
		{"similar import", "core/example.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil_other"`, 0},
		{"comment and string", "core/example.go", "package p\n// import \"github.com/jayyao97/zotigo/internal/testutil/a\"\nconst s = `import \"github.com/jayyao97/zotigo/internal/testutil/a\"`", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, err := forbiddenImports(tc.path, []byte(tc.source))
			if err != nil || len(found) != tc.want {
				t.Fatalf("violations = %v, err = %v; want %d", found, err, tc.want)
			}
			for _, message := range found {
				if !strings.HasPrefix(message, tc.path+":") {
					t.Fatalf("missing source location: %s", message)
				}
			}
		})
	}
}

func TestMalformedImportsFail(t *testing.T) {
	if _, err := forbiddenImports("core/example.go", []byte("package p\nimport (")); err == nil {
		t.Fatal("malformed imports must fail the check")
	}
}
