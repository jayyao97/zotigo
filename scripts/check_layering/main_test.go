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
		{"direct", "internal/app/example.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil"`, 1},
		{"alias", "internal/app/example.go", `package p; import helper "github.com/jayyao97/zotigo/internal/testutil/catalogtest"`, 1},
		{"group", "internal/app/example.go", "package p\nimport (\n . \"github.com/jayyao97/zotigo/internal/testutil/a\"\n _ \"github.com/jayyao97/zotigo/internal/testutil/b\"\n)", 2},
		{"raw", "internal/app/example.go", "package p; import `github.com/jayyao97/zotigo/internal/testutil/a`", 1},
		{"escaped", "internal/app/example.go", `package p; import "github.com/jayyao97/zotigo/internal/test\x75til/a"`, 1},
		{"other platform", "internal/app/example_windows.go", "//go:build windows\n\npackage p\nimport \"github.com/jayyao97/zotigo/internal/testutil/a\"", 1},
		{"test", "internal/app/example_test.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil/a"`, 0},
		{"helper", "internal/testutil/a/helper.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil/b"`, 0},
		{"similar directory", "internal/testutil_other/helper.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil/a"`, 1},
		{"similar import", "internal/app/example.go", `package p; import "github.com/jayyao97/zotigo/internal/testutil_other"`, 0},
		{"comment and string", "internal/app/example.go", "package p\n// import \"github.com/jayyao97/zotigo/internal/testutil/a\"\nconst s = `import \"github.com/jayyao97/zotigo/internal/testutil/a\"`", 0},
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

func TestLayerRules(t *testing.T) {
	const m = "github.com/jayyao97/zotigo/"
	for _, tc := range []struct {
		name, path, imported string
		want                 int
	}{
		{"core must not import internal", "core/acp/embed/x.go", m + "internal/buildinfo", 1},
		{"foundation imports nothing", "core/protocol/x.go", m + "core/config", 1},
		{"foundation exception", "core/acp/embed/x.go", m + "core/executor", 0},
		{"tools must not import agent", "core/tools/builtin/x.go", m + "core/agent", 1},
		{"tools may import executor", "core/tools/builtin/x.go", m + "core/executor", 0},
		{"agent subpackage is not the agent", "core/compaction/x.go", m + "core/agent/prompt", 1},
		{"agent must not import session", "core/agent/x.go", m + "core/session", 1},
		{"agent may import its subpackages", "core/agent/x.go", m + "core/agent/prompt", 0},
		{"session may import agent", "core/session/x.go", m + "core/agent", 0},
		{"tui is a client", "cli/tui/x.go", m + "core/agent", 1},
		{"tui may use the client", "cli/tui/x.go", m + "internal/zotigodclient", 0},
		{"wire must not import the daemon", "internal/zotigod/wire/x.go", m + "internal/zotigod", 1},
		{"wire must not import other product packages", "internal/zotigod/wire/x.go", m + "internal/runtime", 1},
		{"wire may import core", "internal/zotigod/wire/x.go", m + "core/protocol", 0},
		{"zotigod may import wire", "internal/zotigod/x.go", m + "internal/zotigod/wire", 0},
		{"sessionlog must not import the daemon", "internal/zotigod/sessionlog/x.go", m + "internal/zotigod", 1},
		{"sessionlog must not import the daemon client", "internal/zotigod/sessionlog/x.go", m + "internal/zotigodclient", 1},
		{"sessionlog must not import the cli", "internal/zotigod/sessionlog/x.go", m + "cli/tui", 1},
		{"sessionlog may import wire", "internal/zotigod/sessionlog/x.go", m + "internal/zotigod/wire", 0},
		{"sessionlog may import runtime", "internal/zotigod/sessionlog/x.go", m + "internal/runtime", 0},
		{"sessionlog may import core", "internal/zotigod/sessionlog/x.go", m + "core/session", 0},
		{"zotigod may import sessionlog", "internal/zotigod/x.go", m + "internal/zotigod/sessionlog", 0},
		{"sessionlog must not import workerbase", "internal/zotigod/sessionlog/x.go", m + "internal/zotigod/workerbase", 1},
		{"workerbase must not import the daemon", "internal/zotigod/workerbase/x.go", m + "internal/zotigod", 1},
		{"workerbase must not import the daemon client", "internal/zotigod/workerbase/x.go", m + "internal/zotigodclient", 1},
		{"workerbase must not import the cli", "internal/zotigod/workerbase/x.go", m + "cli/tui", 1},
		{"workerbase may import wire", "internal/zotigod/workerbase/x.go", m + "internal/zotigod/wire", 0},
		{"workerbase may import sessionlog", "internal/zotigod/workerbase/x.go", m + "internal/zotigod/sessionlog", 0},
		{"workerbase may import runtime", "internal/zotigod/workerbase/x.go", m + "internal/runtime", 0},
		{"workerbase may import core", "internal/zotigod/workerbase/x.go", m + "core/session", 0},
		{"zotigod may import workerbase", "internal/zotigod/x.go", m + "internal/zotigod/workerbase", 0},
		{"worker must not import the daemon", "internal/zotigod/worker/x.go", m + "internal/zotigod", 1},
		{"worker must not import the daemon client", "internal/zotigod/worker/x.go", m + "internal/zotigodclient", 1},
		{"worker must not import the cli", "internal/zotigod/worker/x.go", m + "cli/tui", 1},
		{"worker may import wire", "internal/zotigod/worker/x.go", m + "internal/zotigod/wire", 0},
		{"worker may import sessionlog", "internal/zotigod/worker/x.go", m + "internal/zotigod/sessionlog", 0},
		{"worker may import workerbase", "internal/zotigod/worker/x.go", m + "internal/zotigod/workerbase", 0},
		{"worker may import wiring", "internal/zotigod/worker/x.go", m + "internal/wiring", 0},
		{"worker may import core", "internal/zotigod/worker/x.go", m + "core/agent", 0},
		{"zotigod may import worker", "internal/zotigod/x.go", m + "internal/zotigod/worker", 0},
		{"wire must not import worker", "internal/zotigod/wire/x.go", m + "internal/zotigod/worker", 1},
		{"sessionlog must not import worker", "internal/zotigod/sessionlog/x.go", m + "internal/zotigod/worker", 1},
		{"workerbase must not import worker", "internal/zotigod/workerbase/x.go", m + "internal/zotigod/worker", 1},
		{"tests are exempt", "core/agent/x_test.go", m + "core/session", 0},
		{"third party ignored", "core/protocol/x.go", "golang.org/x/text", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, err := forbiddenImports(tc.path, []byte("package p; import \""+tc.imported+"\""))
			if err != nil || len(found) != tc.want {
				t.Fatalf("violations = %v, err = %v; want %d", found, err, tc.want)
			}
		})
	}
}

func TestMalformedImportsFail(t *testing.T) {
	if _, err := forbiddenImports("internal/app/example.go", []byte("package p\nimport (")); err == nil {
		t.Fatal("malformed imports must fail the check")
	}
}
