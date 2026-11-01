package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/internal/buildinfo"
)

func TestReleaseManifestMatchesBinary(t *testing.T) {
	manifest, err := readManifest(filepath.Join("..", "..", "update.json"))
	if err != nil || manifest.Version != buildinfo.Version {
		t.Fatalf("manifest %v: %v", manifest, err)
	}
	if ref := os.Getenv("GITHUB_REF"); strings.HasPrefix(ref, "refs/tags/v") && strings.TrimPrefix(ref, "refs/tags/v") != manifest.Version {
		t.Fatalf("release tag %s does not match manifest", ref)
	}
}

func TestReleaseChannelOrdering(t *testing.T) {
	sha := strings.Repeat("a", 40)
	tags := ""
	for _, tag := range []string{"v1.2.0-dev.9", "v1.2.0-dev.10", "v1.1.9", "v1.2.0-rc.99", "master"} {
		tags += sha + " refs/tags/" + tag + "\n"
	}
	for channel, want := range map[string]string{"stable": "v1.1.9", "development": "v1.2.0-dev.10"} {
		got, _, err := SelectRelease(tags, channel)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", channel, got, err)
		}
	}
	tags += sha + " refs/tags/v1.2.0\n"
	if got, _, _ := SelectRelease(tags, "development"); got != "v1.2.0" {
		t.Fatal(got)
	}
	if _, _, err := SelectRelease(tags, "master"); err == nil {
		t.Fatal("accepted arbitrary channel")
	}
}

func fixtureManager(t *testing.T) *Manager {
	t.Helper()
	prefix := t.TempDir()
	current := filepath.Join(prefix, "daemon", "current")
	if err := os.MkdirAll(current, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "INSTALLATION"), []byte("version=1.2.0-dev.10\ncommit=current\ndirty=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(prefix, "daemon")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestChannelPersistsPerInstallationAndRejectsConcurrentMutation(t *testing.T) {
	m := fixtureManager(t)
	s, err := m.Status()
	if err != nil || s.Channel != "development" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := m.SetChannel("stable"); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := New(m.Prefix, "daemon")
	s, _ = reloaded.Status()
	if s.Channel != "stable" {
		t.Fatal(s)
	}
	other := fixtureManager(t)
	s, _ = other.Status()
	if s.Channel != "development" {
		t.Fatal("channel leaked between hosts")
	}
	if err := m.locked(func() error {
		_, err := reloaded.SetChannel("development")
		if err == nil {
			t.Fatal("concurrent update accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckExplicitStableMigrationAndNetworkFailure(t *testing.T) {
	m := fixtureManager(t)
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	git("init", "-q")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "release")
	git("tag", "v1.1.0")
	git("tag", "v1.2.0-dev.11")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+repo+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", m.repository())
	if _, err := m.SetChannel("stable"); err != nil {
		t.Fatal(err)
	}
	s, err := m.Check(context.Background())
	if err != nil || s.Target == nil || s.Target.Version != "1.1.0" {
		t.Fatalf("stable migration: %+v %v", s, err)
	}
	if _, err := m.SetChannel("development"); err != nil {
		t.Fatal(err)
	}
	s, err = m.Check(context.Background())
	if err != nil || s.Target == nil || s.Target.Version != "1.2.0-dev.11" {
		t.Fatalf("development update: %+v %v", s, err)
	}
	t.Setenv("GIT_CONFIG_KEY_0", "url."+filepath.Join(repo, "missing")+".insteadOf")
	if _, err = m.Check(context.Background()); err == nil {
		t.Fatal("network failure reported as up-to-date")
	}
}

func TestRecoveryRestoresMissingCurrentAndClearsAdmission(t *testing.T) {
	m := fixtureManager(t)
	stage := filepath.Join(m.directory(), "apply.fixture")
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(m.current(""), filepath.Join(stage, "old-current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.directory(), "journal"), []byte(stage), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.save(Status{Channel: "development", Phase: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginDrain(); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(m.Prefix, "daemon")
	if err != nil {
		t.Fatal(err)
	}
	s, err := recovered.Status()
	if err != nil || s.Phase != "failed" || s.Version != "1.2.0-dev.10" || recovered.Draining() {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestRecoveryNeverInterferesWithLiveWorker(t *testing.T) {
	m := fixtureManager(t)
	if err := m.save(Status{Channel: "stable", Phase: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginDrain(); err != nil {
		t.Fatal(err)
	}
	if err := m.locked(func() error {
		if err := m.Recover(true); err != nil {
			t.Fatal(err)
		}
		if !m.Draining() {
			t.Fatal("cleared active worker drain")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(false); err != nil {
		t.Fatal(err)
	}
	if m.Draining() {
		t.Fatal("stale drain persisted")
	}
}
