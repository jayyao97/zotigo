package builtin

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jayyao97/zotigo/core/tools"
)

// ShellPolicy is a command-string matcher used by ShellTool to decide
// how risky a proposed shell invocation looks. It's a linter, not a
// real sandbox — it matches regex patterns against the raw command and
// yields a SafetyLevel the shell tool can return straight out of
// Classify.
//
// The patterns are a coarse first line of defense: they should only
// cover operations that are catastrophic regardless of context
// (rm -rf /, fork bomb, disk dd, mkfs). Anything ambiguous is better
// left to the LLM safety classifier, which has the user prompt and
// history to make a contextual call.
type ShellPolicy struct {
	// BlockedPatterns — matching commands are hard-refused. Returned
	// as tools.LevelBlocked.
	BlockedPatterns []string `json:"blocked_patterns" yaml:"blocked_patterns"`

	// HighRiskPatterns — matching commands surface as tools.LevelHigh
	// so the agent will always route them through the classifier
	// (Auto mode) or user approval (Manual mode).
	HighRiskPatterns []string `json:"high_risk_patterns" yaml:"high_risk_patterns"`

	// AllowedCommands — if non-empty, only commands whose base verb
	// is in this list are allowed. Anything else returns
	// tools.LevelBlocked with a "not in whitelist" reason. Off by
	// default (no whitelist).
	AllowedCommands []string `json:"allowed_commands,omitempty" yaml:"allowed_commands,omitempty"`

	blockedRegexps, highRiskRegexps []*regexp.Regexp
}

// DefaultShellPolicy returns a conservative policy focused on commands
// that are destructive under any interpretation. Patterns that depend
// on context (sudo, pip install, systemctl, ...) are intentionally
// omitted — the safety classifier handles those with better judgment.
func DefaultShellPolicy() *ShellPolicy {
	return &ShellPolicy{
		BlockedPatterns: []string{
			`rm\s+(-[rf]+\s+)*/(etc|var|usr|bin|sbin|boot|lib|root|home)`,
			`rm\s+-rf\s+/\s*$`,
			`rm\s+-rf\s+/\*`,
			`rm\s+-rf\s+~`,
			`>\s*/dev/(sd|hd|nvme|disk|vd)[a-z]`,
			`dd\s+.*of=/dev/(sd|hd|nvme|disk|vd)`,
			`mkfs\.`,
			`:\(\)\s*\{\s*:\|:\&\s*\};:`, // fork bomb
			`chmod\s+(-R\s+)?777\s+/`,
			`chown\s+(-R\s+)?.*\s+/`,
		},
		HighRiskPatterns: nil,
	}
}

// Compile turns the pattern strings into regexps. Must be called before
// Classify (NewShellTool with WithPolicy handles this automatically).
func (p *ShellPolicy) Compile() error {
	p.blockedRegexps = make([]*regexp.Regexp, 0, len(p.BlockedPatterns))
	for _, pat := range p.BlockedPatterns {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return fmt.Errorf("blocked pattern %q: %w", pat, err)
		}
		p.blockedRegexps = append(p.blockedRegexps, re)
	}
	p.highRiskRegexps = make([]*regexp.Regexp, 0, len(p.HighRiskPatterns))
	for _, pat := range p.HighRiskPatterns {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return fmt.Errorf("high-risk pattern %q: %w", pat, err)
		}
		p.highRiskRegexps = append(p.highRiskRegexps, re)
	}
	return nil
}

// Classify evaluates a command and returns the SafetyLevel the shell
// tool should surface, plus a short human-readable reason.
//
// The reason field disambiguates the two meanings of LevelSafe:
//
//   - (LevelSafe, "") — policy has no opinion. Caller should apply its
//     own heuristics (e.g. read-only verb whitelist).
//   - (LevelSafe, non-empty) — policy explicitly accepted the command
//     (AllowedCommands hit). Caller should treat this as definitive.
func (p *ShellPolicy) Classify(cmd string) (tools.SafetyLevel, string) {
	for i, re := range p.blockedRegexps {
		if re.MatchString(cmd) {
			return tools.LevelBlocked, "blocked by pattern: " + p.BlockedPatterns[i]
		}
	}
	var whitelistHit bool
	if len(p.AllowedCommands) > 0 {
		base := extractBaseCommand(cmd)
		allowed := false
		for _, ac := range p.AllowedCommands {
			if base == ac {
				allowed = true
				break
			}
		}
		if !allowed {
			return tools.LevelBlocked, "command not in allowed list: " + base
		}
		whitelistHit = true
	}
	for i, re := range p.highRiskRegexps {
		if re.MatchString(cmd) {
			return tools.LevelHigh, "matched high-risk pattern: " + p.HighRiskPatterns[i]
		}
	}
	if whitelistHit {
		return tools.LevelSafe, "matched allowed-commands whitelist"
	}
	return tools.LevelSafe, ""
}

// extractBaseCommand returns the command verb (e.g. "rm", "git"),
// peeling off common wrappers like env/time/nice. Returns "" when the
// input is empty or is purely wrappers with no real verb (e.g.
// `env FOO=bar` with no command after) — the caller should treat an
// empty verb as "no executable command", not as a name to whitelist.
func extractBaseCommand(cmd string) string {
	parts := strings.Fields(strings.TrimSpace(cmd))
	if len(parts) == 0 {
		return ""
	}

	i := 0
	for i < len(parts) {
		base := filepath.Base(parts[i])
		switch base {
		case "env":
			i++
			for i < len(parts) && strings.Contains(parts[i], "=") {
				i++
			}
			continue
		case "time", "nohup":
			i++
			continue
		case "nice":
			i++
			for i < len(parts) && strings.HasPrefix(parts[i], "-") {
				i++
				if i < len(parts) && !strings.HasPrefix(parts[i], "-") {
					i++
				}
			}
			continue
		}
		return base
	}
	return ""
}
