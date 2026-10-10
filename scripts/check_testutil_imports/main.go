// Command check_testutil_imports keeps test helpers out of production imports.
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

const testutilImport = "github.com/jayyao97/zotigo/internal/testutil"

func forbiddenImports(path string, source []byte) ([]string, error) {
	if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "internal/testutil/") {
		return nil, nil
	}
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
		if name == testutilImport || strings.HasPrefix(name, testutilImport+"/") {
			violations = append(violations, fmt.Sprintf("%s: test-only import %q is forbidden outside tests and internal/testutil", fset.Position(spec.Pos()), name))
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
