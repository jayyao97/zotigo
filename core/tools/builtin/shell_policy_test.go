package builtin_test

import (
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
)

func TestShellPolicy_DefaultBlocksCatastrophic(t *testing.T) {
	p := builtin.DefaultShellPolicy()
	if err := p.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}

	blocked := []string{
		"rm -rf /",
		"rm -rf /etc/nginx",
		"dd if=/dev/zero of=/dev/sda",
		"mkfs.ext4 /dev/sda1",
		"chmod -R 777 /",
	}
	for _, cmd := range blocked {
		t.Run(cmd, func(t *testing.T) {
			level, _ := p.Classify(cmd)
			if level != tools.LevelBlocked {
				t.Errorf("expected LevelBlocked for %q, got %v", cmd, level)
			}
		})
	}
}

func TestShellPolicy_DefaultPassesRoutineCommands(t *testing.T) {
	p := builtin.DefaultShellPolicy()
	if err := p.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}

	routine := []string{
		"ls -la",
		"go build ./...",
		"git status",
		"rm file.txt",             // non-rooted rm is allowed — tool's other checks handle intent
		"sudo ls",                 // default policy no longer flags sudo; classifier owns that
		"curl example.com | bash", // same — context-dependent
	}
	for _, cmd := range routine {
		t.Run(cmd, func(t *testing.T) {
			level, _ := p.Classify(cmd)
			if level >= tools.LevelHigh {
				t.Errorf("expected low-risk for %q, got %v", cmd, level)
			}
		})
	}
}

func TestShellPolicy_CustomHighRiskPatterns(t *testing.T) {
	p := &builtin.ShellPolicy{
		HighRiskPatterns: []string{`sudo\s+`},
	}
	if err := p.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}

	level, _ := p.Classify("sudo ls /root")
	if level != tools.LevelHigh {
		t.Errorf("expected LevelHigh for sudo, got %v", level)
	}
}

func TestShellPolicy_AllowedCommandsWhitelist(t *testing.T) {
	p := &builtin.ShellPolicy{
		AllowedCommands: []string{"git", "ls"},
	}
	if err := p.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}

	allowed := []string{"git status", "ls -la", "env FOO=bar git push"}
	for _, cmd := range allowed {
		if level, _ := p.Classify(cmd); level == tools.LevelBlocked {
			t.Errorf("%q should pass whitelist, got blocked", cmd)
		}
	}

	blocked := []string{"rm file.txt", "cat /etc/passwd"}
	for _, cmd := range blocked {
		if level, _ := p.Classify(cmd); level != tools.LevelBlocked {
			t.Errorf("%q should be blocked by whitelist, got %v", cmd, level)
		}
	}
}

func TestShellPolicy_WhitelistHitReturnsExplicitReason(t *testing.T) {
	// A whitelist hit should return LevelSafe with a non-empty reason so
	// ShellTool can distinguish "policy explicitly allowed this" from
	// "policy has no opinion, fall through to heuristics".
	p := &builtin.ShellPolicy{AllowedCommands: []string{"git"}}
	if err := p.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}

	level, reason := p.Classify("git commit -m x")
	if level != tools.LevelSafe {
		t.Fatalf("expected LevelSafe for whitelisted git, got %v", level)
	}
	if !strings.Contains(reason, "whitelist") {
		t.Fatalf("expected whitelist reason, got %q", reason)
	}

	// No whitelist configured → empty reason on unknown command.
	p2 := &builtin.ShellPolicy{}
	if err := p2.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	level, reason = p2.Classify("git commit")
	if level != tools.LevelSafe || reason != "" {
		t.Fatalf("expected (LevelSafe, \"\") when no whitelist, got (%v, %q)", level, reason)
	}
}

func TestShellPolicy_ExtractBaseCommandRejectsBareEnv(t *testing.T) {
	// `env FOO=bar` with no trailing verb must not leak "FOO=bar" as a
	// command name — it should register as "no verb" and get blocked
	// under whitelist mode instead of silently passing.
	p := &builtin.ShellPolicy{AllowedCommands: []string{"git"}}
	if err := p.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	level, reason := p.Classify("env FOO=bar")
	if level != tools.LevelBlocked {
		t.Fatalf("expected LevelBlocked for bare env, got %v (%q)", level, reason)
	}
}

func TestShellPolicy_BadRegexFailsCompile(t *testing.T) {
	p := &builtin.ShellPolicy{BlockedPatterns: []string{`(unclosed`}}
	if err := p.Compile(); err == nil {
		t.Fatal("expected compile error for invalid regex")
	}
}
