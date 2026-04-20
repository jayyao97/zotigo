package builtin_test

import (
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

func TestShellPolicy_BadRegexFailsCompile(t *testing.T) {
	p := &builtin.ShellPolicy{BlockedPatterns: []string{`(unclosed`}}
	if err := p.Compile(); err == nil {
		t.Fatal("expected compile error for invalid regex")
	}
}
