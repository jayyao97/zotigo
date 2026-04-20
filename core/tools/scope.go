package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// IsInWorkDir reports whether every path-shaped argument in the JSON call
// resolves to a location under the working directory. Missing keys are
// treated as absent (not a violation). Non-string values are ignored.
//
// Use this for "writes into the project" checks — acceptEdits-style
// auto-approval where the extra safe directories should not apply.
func IsInWorkDir(call SafetyCall, pathKeys []string) bool {
	paths := extractPaths(call.Arguments, pathKeys)
	if len(paths) == 0 {
		return true
	}
	workDir := filepath.Clean(call.WorkDir)
	for _, p := range paths {
		if !hasPrefix(resolve(p, workDir), workDir) {
			return false
		}
	}
	return true
}

// IsInSafeScope reports whether every path-shaped argument resolves to a
// location under any of the agent's safe directories (working directory
// plus extras such as active skills). Use this for read-style checks.
func IsInSafeScope(call SafetyCall, pathKeys []string) bool {
	paths := extractPaths(call.Arguments, pathKeys)
	if len(paths) == 0 {
		return true
	}
	dirs := call.SafeDirs
	if len(dirs) == 0 {
		dirs = []string{call.WorkDir}
	}
	cleaned := make([]string, 0, len(dirs))
	for _, d := range dirs {
		cleaned = append(cleaned, filepath.Clean(d))
	}
	workDir := filepath.Clean(call.WorkDir)
	for _, p := range paths {
		abs := resolve(p, workDir)
		inScope := false
		for _, d := range cleaned {
			if hasPrefix(abs, d) {
				inScope = true
				break
			}
		}
		if !inScope {
			return false
		}
	}
	return true
}

// IsSensitivePath returns true if any path-shaped argument resolves to a
// known-sensitive location: credentials, shell config, VCS internals, SSH
// keys, etc. Tools use this to force approval/classifier even when the
// path is inside the working directory.
func IsSensitivePath(call SafetyCall, pathKeys []string) bool {
	for _, p := range extractPaths(call.Arguments, pathKeys) {
		if isSensitive(p) {
			return true
		}
	}
	return false
}

// StringArg returns the string value of a JSON argument key, or "" if
// missing or not a string.
func StringArg(argsJSON, key string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func resolve(p, workDir string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(workDir, p)
	}
	return filepath.Clean(p)
}

func hasPrefix(abs, dir string) bool {
	if abs == dir {
		return true
	}
	return strings.HasPrefix(abs, dir+string(filepath.Separator))
}

// extractPaths pulls string values for each requested key from the
// JSON arguments object. Missing/non-string values are skipped.
func extractPaths(argsJSON string, pathKeys []string) []string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil
	}
	out := make([]string, 0, len(pathKeys))
	for _, key := range pathKeys {
		v, ok := args[key]
		if !ok || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// isSensitive reports whether a single path string points to a known
// sensitive file. Kept here (not exported) so callers go through the
// IsSensitivePath(call, pathKeys) helper.
func isSensitive(path string) bool {
	norm := strings.ToLower(filepath.ToSlash(path))

	// Basename-level matches: shell config, credentials, keys, dotenv.
	base := filepath.Base(norm)
	switch base {
	case ".bashrc", ".bash_profile", ".bash_login", ".bash_logout",
		".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout",
		".profile", ".cshrc", ".tcshrc", ".fish",
		".gitconfig", ".gitignore_global",
		".netrc", ".pgpass", ".my.cnf",
		".npmrc", ".pypirc",
		".env", ".env.local", ".env.production", ".env.development",
		"credentials", "credentials.json",
		"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519":
		return true
	}

	// Directory-segment matches: anything inside these paths is sensitive.
	for _, seg := range strings.Split(norm, "/") {
		switch seg {
		case ".git", ".svn", ".hg",
			".ssh", ".gnupg",
			".aws", ".kube", ".docker":
			return true
		}
	}
	return false
}
