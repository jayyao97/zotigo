package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type SourceInspection struct {
	Kind            SourceKind `json:"kind"`
	CanonicalPath   string     `json:"canonical_path"`
	GitCommonDir    string     `json:"git_common_dir,omitempty"`
	GitObjectFormat string     `json:"git_object_format,omitempty"`
	SourceKey       string     `json:"source_key"`
}

func InspectSource(ctx context.Context, path string) (SourceInspection, error) {
	if path == "" || !filepath.IsAbs(path) {
		return SourceInspection{}, fmt.Errorf("%w: source path must be absolute", ErrInvalid)
	}
	canonicalPath, err := canonicalDirectory(path)
	if err != nil {
		return SourceInspection{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, gitErr := runGit(gitCtx, canonicalPath,
		"rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir", "--show-object-format")
	if gitErr == nil {
		lines := strings.Split(strings.TrimSpace(output), "\n")
		if len(lines) != 3 {
			return SourceInspection{}, fmt.Errorf("inspect git source: unexpected rev-parse output")
		}
		root, err := canonicalDirectory(lines[0])
		if err != nil {
			return SourceInspection{}, fmt.Errorf("inspect git root: %w", err)
		}
		commonDir, err := canonicalDirectory(lines[1])
		if err != nil {
			return SourceInspection{}, fmt.Errorf("inspect git common dir: %w", err)
		}
		objectFormat := strings.TrimSpace(lines[2])
		if objectFormat != "sha1" && objectFormat != "sha256" {
			return SourceInspection{}, fmt.Errorf("inspect git source: unsupported object format %q", objectFormat)
		}
		return SourceInspection{
			Kind:            SourceKindGit,
			CanonicalPath:   root,
			GitCommonDir:    commonDir,
			GitObjectFormat: objectFormat,
			SourceKey:       sourceKey(SourceKindGit, commonDir),
		}, nil
	}
	if gitCtx.Err() != nil {
		return SourceInspection{}, fmt.Errorf("inspect git source: %w", gitCtx.Err())
	}
	return SourceInspection{
		Kind:          SourceKindFolder,
		CanonicalPath: canonicalPath,
		SourceKey:     sourceKey(SourceKindFolder, canonicalPath),
	}, nil
}

func verifySourceIdentity(ctx context.Context, source Source) error {
	inspection, err := InspectSource(ctx, source.CanonicalPath)
	if err != nil {
		return err
	}
	if inspection.Kind != source.Kind || inspection.CanonicalPath != source.CanonicalPath ||
		inspection.GitCommonDir != source.GitCommonDir || inspection.GitObjectFormat != source.GitObjectFormat {
		return fmt.Errorf("%w: source identity changed", ErrConflict)
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return filepath.Clean(abs), nil
}

func sourceKey(kind SourceKind, identity string) string {
	digest := sha256.Sum256([]byte(string(kind) + "\x00" + identity))
	return hex.EncodeToString(digest[:8])
}

func runGit(ctx context.Context, directory string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", directory}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = sanitizedGitEnvironment(os.Environ())
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(output) > 64*1024 {
		return "", fmt.Errorf("git output exceeded limit")
	}
	return string(output), nil
}

func sanitizedGitEnvironment(environment []string) []string {
	blocked := []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_COMMON_DIR=", "GIT_INDEX_FILE=",
		"GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=",
	}
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		skip := false
		for _, prefix := range blocked {
			if strings.HasPrefix(entry, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			result = append(result, entry)
		}
	}
	return append(result, "GIT_TERMINAL_PROMPT=0")
}
