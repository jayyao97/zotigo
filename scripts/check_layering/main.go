// Command check_layering enforces the package dependency rules described in
// docs/architecture.md. Only production sources are checked; tests may import
// upward to build fixtures.
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const module = "github.com/jayyao97/zotigo/"

// A rule forbids packages under from importing packages under deny.
// Entries are repo-relative package paths; each also matches its subpackages.
type rule struct {
	from   []string
	deny   []string
	allow  []string // packages under deny that may be imported anyway
	except []string // packages under from that the rule does not apply to
	why    string
}

var rules = []rule{
	{
		from: []string{""}, except: []string{"internal/testutil"},
		deny: []string{"internal/testutil"},
		why:  "test-only import is forbidden outside tests and internal/testutil",
	},
	{
		from: []string{"core"},
		deny: []string{"internal", "cli", "cmd"},
		why:  "core is the embeddable library and must not depend on the product",
	},
	{
		from:   []string{"core/protocol", "core/config", "core/executor", "core/debug", "core/skills", "core/lsp", "core/catalogschema", "core/acp", "core/agent/prompt", "core/agent/loopguard"},
		except: []string{"core/acp/embed"},
		deny:   []string{"core", "internal", "cli", "cmd"},
		why:    "foundation packages import no other zotigo package",
	},
	{
		from: []string{"core/tools", "core/providers", "core/compaction", "core/transport", "core/observability", "core/textassist", "core/workspace"},
		deny: []string{"core/agent", "core/session", "core/runner", "core/subagent", "core/middleware"},
		why:  "capabilities are used by the agent and must not know about it",
	},
	{
		from: []string{"core/agent"},
		deny: []string{"core/session", "core/runner", "core/subagent", "core/middleware", "core/transport", "core/workspace", "core/skills", "core/acp"},
		why:  "the agent kernel does not know how it is hosted, persisted or driven",
	},
	{
		from: []string{"cli"},
		deny: []string{"core/agent", "internal/zotigod", "internal/wiring"},
		why:  "the TUI is a client of the session service and never runs an agent itself",
	},
	{
		from: []string{"internal/zotigod/wire"},
		deny: []string{"internal", "cli", "cmd"},
		why:  "the daemon-worker protocol is data both processes share and must not depend on either side",
	},
	{
		from:  []string{"internal/zotigod/sessionlog"},
		deny:  []string{"internal/zotigod", "internal/zotigodclient", "internal/cliapp", "internal/acpserver", "cli", "cmd"},
		allow: []string{"internal/zotigod/wire"},
		why:   "the session log is run by both the daemon and its workers and must not depend on the daemon or its clients",
	},
	{
		from:  []string{"internal/zotigod/workerbase"},
		deny:  []string{"internal/zotigod", "internal/zotigodclient", "internal/cliapp", "internal/acpserver", "cli", "cmd"},
		allow: []string{"internal/zotigod/wire", "internal/zotigod/sessionlog"},
		why:   "the worker base is linked into every worker process and must not depend on the daemon or its clients",
	},
}

func under(pkg string, roots []string) bool {
	for _, root := range roots {
		if root == "" || pkg == root || strings.HasPrefix(pkg, root+"/") {
			return true
		}
	}
	return false
}

// sameTree reports whether imported is pkg or one of its subpackages, which
// a package may always import.
func sameTree(pkg, imported string) bool {
	return imported == pkg || strings.HasPrefix(imported, pkg+"/")
}

func forbiddenImports(path string, source []byte) ([]string, error) {
	if strings.HasSuffix(path, "_test.go") {
		return nil, nil
	}
	pkg := filepath.ToSlash(filepath.Dir(path))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var violations []string
	for _, spec := range file.Imports {
		name, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(name, module) {
			continue
		}
		imported := strings.TrimPrefix(name, module)
		for _, r := range rules {
			if !under(pkg, r.from) || under(pkg, r.except) || !under(imported, r.deny) || under(imported, r.allow) || sameTree(pkg, imported) {
				continue
			}
			violations = append(violations, fmt.Sprintf("%s: %s must not import %q: %s", fset.Position(spec.Pos()), pkg, name, r.why))
			break
		}
	}
	return violations, nil
}

func run() error {
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("find repository: %w", err)
	}
	root := strings.TrimSpace(string(output))
	// Include untracked additions for local checks, and inspect all build tags
	// and platforms rather than only files selected by the current Go build.
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go")
	cmd.Dir = root
	output, err = cmd.Output()
	if err != nil {
		return fmt.Errorf("list Go sources: %w", err)
	}
	var violations []string
	for _, path := range strings.Split(string(output), "\x00") {
		if path == "" {
			continue
		}
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		found, err := forbiddenImports(path, source)
		if err != nil {
			return err
		}
		violations = append(violations, found...)
	}
	if len(violations) != 0 {
		return fmt.Errorf("%s", strings.Join(violations, "\n"))
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
