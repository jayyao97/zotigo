// Package sandbox provides security policies for command execution and file operations.
package sandbox

import (
	"path/filepath"
	"regexp"
	"strings"
)

// RiskLevel indicates the risk level of a command.
type RiskLevel int

const (
	RiskLevelSafe     RiskLevel = iota // Command is safe to execute
	RiskLevelNormal                    // Normal command, standard approval
	RiskLevelHigh                      // High risk, requires extra confirmation
	RiskLevelBlocked                   // Blocked, cannot execute
)

// String returns the string representation of RiskLevel.
func (r RiskLevel) String() string {
	switch r {
	case RiskLevelSafe:
		return "safe"
	case RiskLevelNormal:
		return "normal"
	case RiskLevelHigh:
		return "high"
	case RiskLevelBlocked:
		return "blocked"
	}
	return "unknown"
}

// Policy defines security rules for command execution and file access.
type Policy struct {
	// Blocked patterns - commands matching these are completely forbidden
	BlockedPatterns []string `json:"blocked_patterns" yaml:"blocked_patterns"`

	// HighRisk patterns - commands matching these require extra confirmation
	HighRiskPatterns []string `json:"high_risk_patterns" yaml:"high_risk_patterns"`

	// AllowedCommands - if non-empty, only these base commands are allowed (whitelist mode)
	AllowedCommands []string `json:"allowed_commands,omitempty" yaml:"allowed_commands,omitempty"`

	// AllowedPaths - directories where file operations are permitted
	// If empty, defaults to workDir only
	AllowedPaths []string `json:"allowed_paths,omitempty" yaml:"allowed_paths,omitempty"`

	// AllowNetworkAccess - whether to allow network-related commands
	AllowNetworkAccess bool `json:"allow_network_access" yaml:"allow_network_access"`

	// compiled patterns (internal)
	blockedRegexps  []*regexp.Regexp
	highRiskRegexps []*regexp.Regexp
}

// DefaultPolicy returns a sensible default security policy.
func DefaultPolicy() *Policy {
	return &Policy{
		BlockedPatterns: []string{
			// Destructive file operations
			`rm\s+(-[rf]+\s+)*/(etc|var|usr|bin|sbin|boot|lib|root|home)`,
			`rm\s+-rf\s+/\s*$`,                     // rm -rf /
			`rm\s+-rf\s+/\*`,                       // rm -rf /*
			`rm\s+-rf\s+~`,                         // rm -rf ~
			`>\s*/dev/(sd|hd|nvme|disk|vd)[a-z]`,   // overwrite disk devices
			`dd\s+.*of=/dev/(sd|hd|nvme|disk|vd)`,  // dd to disk
			`mkfs\.`,                               // format filesystem
			`:(){ :|:& };:`,                        // fork bomb
			`chmod\s+(-R\s+)?777\s+/`,              // chmod 777 /
			`chown\s+(-R\s+)?.*\s+/`,               // chown /
		},
		HighRiskPatterns: []string{
			// Remote code execution
			`curl\s+.*\|\s*(sh|bash|zsh|python|perl|ruby)`,
			`wget\s+.*\|\s*(sh|bash|zsh|python|perl|ruby)`,
			`curl\s+.*>\s*.*\.sh\s*&&`,
			`wget\s+.*-O\s*.*\.sh\s*&&`,

			// Privileged operations
			`sudo\s+`,
			`su\s+-`,
			`doas\s+`,

			// System modification
			`systemctl\s+(start|stop|restart|enable|disable)`,
			`service\s+.*\s+(start|stop|restart)`,
			`launchctl\s+`,

			// Package managers with install (could install malicious packages)
			`pip\s+install\s+.*--`,
			`npm\s+install\s+-g`,
			`gem\s+install`,

			// Git operations that modify remote
			`git\s+push\s+.*--force`,
			`git\s+push\s+-f`,

			// Environment modification
			`export\s+PATH=`,
			`unset\s+PATH`,
		},
		AllowedCommands:    nil, // No whitelist by default
		AllowedPaths:       nil, // Will default to workDir
		AllowNetworkAccess: true,
	}
}

// StrictPolicy returns a more restrictive policy.
func StrictPolicy() *Policy {
	p := DefaultPolicy()
	p.AllowedCommands = []string{
		// Version control
		"git",
		// Build tools
		"go", "cargo", "npm", "yarn", "pnpm", "pip", "python", "python3",
		"node", "deno", "bun",
		"make", "cmake", "gradle", "mvn",
		// Common utilities
		"cat", "head", "tail", "less", "more",
		"grep", "rg", "ag", "ack",
		"find", "fd", "ls", "tree",
		"wc", "sort", "uniq", "diff",
		"echo", "printf", "date",
		"mkdir", "cp", "mv", "touch",
		// Editors (for scripted use)
		"sed", "awk",
	}
	p.AllowNetworkAccess = false
	return p
}

// Compile compiles the regex patterns. Must be called before use.
func (p *Policy) Compile() error {
	p.blockedRegexps = make([]*regexp.Regexp, 0, len(p.BlockedPatterns))
	for _, pattern := range p.BlockedPatterns {
		re, err := regexp.Compile("(?i)" + pattern) // case insensitive
		if err != nil {
			return err
		}
		p.blockedRegexps = append(p.blockedRegexps, re)
	}

	p.highRiskRegexps = make([]*regexp.Regexp, 0, len(p.HighRiskPatterns))
	for _, pattern := range p.HighRiskPatterns {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return err
		}
		p.highRiskRegexps = append(p.highRiskRegexps, re)
	}

	return nil
}

// CheckCommand evaluates a command against the policy.
func (p *Policy) CheckCommand(cmd string) (RiskLevel, string) {
	// Check blocked patterns first
	for i, re := range p.blockedRegexps {
		if re.MatchString(cmd) {
			return RiskLevelBlocked, p.BlockedPatterns[i]
		}
	}

	// Check command whitelist if enabled
	if len(p.AllowedCommands) > 0 {
		baseCmd := extractBaseCommand(cmd)
		allowed := false
		for _, allowedCmd := range p.AllowedCommands {
			if baseCmd == allowedCmd {
				allowed = true
				break
			}
		}
		if !allowed {
			return RiskLevelBlocked, "command not in whitelist: " + baseCmd
		}
	}

	// Check high risk patterns
	for i, re := range p.highRiskRegexps {
		if re.MatchString(cmd) {
			return RiskLevelHigh, p.HighRiskPatterns[i]
		}
	}

	return RiskLevelNormal, ""
}

// CheckPath verifies if a path is within allowed directories.
func (p *Policy) CheckPath(path string, workDir string) (bool, string) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, "invalid path"
	}

	// Clean the path to prevent traversal attacks
	absPath = filepath.Clean(absPath)

	// Build allowed paths list
	allowed := p.AllowedPaths
	if len(allowed) == 0 {
		allowed = []string{workDir}
	}

	// Always allow /tmp for temporary files
	allowed = append(allowed, "/tmp", "/var/tmp")

	for _, allowedPath := range allowed {
		absAllowed, err := filepath.Abs(allowedPath)
		if err != nil {
			continue
		}
		absAllowed = filepath.Clean(absAllowed)

		// Check if path is under allowed directory
		if strings.HasPrefix(absPath, absAllowed+string(filepath.Separator)) || absPath == absAllowed {
			return true, ""
		}
	}

	return false, "path not in allowed directories: " + absPath
}

// extractBaseCommand extracts the base command name from a command string.
func extractBaseCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)

	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}

	i := 0
	for i < len(parts) {
		part := parts[i]
		base := filepath.Base(part)

		// Skip common wrapper commands
		switch base {
		case "env":
			i++
			// Skip VAR=value assignments after env
			for i < len(parts) && strings.Contains(parts[i], "=") {
				i++
			}
			continue
		case "time", "nohup":
			i++
			continue
		case "nice":
			i++
			// Skip nice options like -n 10
			for i < len(parts) && strings.HasPrefix(parts[i], "-") {
				i++
				// Skip the value after -n
				if i < len(parts) && !strings.HasPrefix(parts[i], "-") {
					i++
				}
			}
			continue
		}

		// Found the actual command
		return base
	}

	// Fallback to last part if all were prefixes
	if len(parts) > 0 {
		return filepath.Base(parts[len(parts)-1])
	}
	return ""
}
