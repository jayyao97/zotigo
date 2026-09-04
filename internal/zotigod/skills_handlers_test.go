package zotigod

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/internal/sessionadapter"
)

func TestSkillsAPIListsUserAndSessionWorkspaceSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeDaemonTestSkill(t, filepath.Join(home, ".agents", "skills"), "shared", "user version", "user instructions")
	writeDaemonTestSkill(t, filepath.Join(home, ".agents", "skills"), "user-only", "user only", "user instructions")
	workspace := t.TempDir()
	writeDaemonTestSkill(t, filepath.Join(workspace, ".agents", "skills"), "shared", "workspace version", "workspace instructions")
	invalidDir := filepath.Join(workspace, ".agents", "skills", "invalid")
	if err := os.MkdirAll(invalidDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidDir, "SKILL.md"), []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}

	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	withoutSession := httptest.NewRecorder()
	handler.ServeHTTP(withoutSession, httptest.NewRequest(http.MethodGet, "/skills", nil))
	if withoutSession.Code != http.StatusOK {
		t.Fatalf("GET /skills = %d: %s", withoutSession.Code, withoutSession.Body.String())
	}
	var userResult skillsResponse
	if err := decodeAPIData(t, withoutSession.Body.Bytes(), &userResult); err != nil {
		t.Fatal(err)
	}
	if skill := findPublicSkill(userResult.Skills, "user-only"); skill == nil || skill.Scope != "user" {
		t.Fatalf("user skills = %#v", userResult.Skills)
	}
	if skill := findPublicSkill(userResult.Skills, "shared"); skill == nil || skill.Description != "user version" {
		t.Fatalf("shared user skill = %#v", skill)
	}
	writeDaemonTestSkill(t, filepath.Join(home, ".agents", "skills"), "late", "late", "late instructions")
	cached := httptest.NewRecorder()
	handler.ServeHTTP(cached, httptest.NewRequest(http.MethodGet, "/skills", nil))
	var cachedResult skillsResponse
	if err := decodeAPIData(t, cached.Body.Bytes(), &cachedResult); err != nil {
		t.Fatal(err)
	}
	if findPublicSkill(cachedResult.Skills, "late") != nil {
		t.Fatal("cached skills unexpectedly rescanned the filesystem")
	}
	reloaded := httptest.NewRecorder()
	handler.ServeHTTP(reloaded, httptest.NewRequest(http.MethodGet, "/skills?force_reload=true", nil))
	var reloadedResult skillsResponse
	if err := decodeAPIData(t, reloaded.Body.Bytes(), &reloadedResult); err != nil {
		t.Fatal(err)
	}
	if findPublicSkill(reloadedResult.Skills, "late") == nil {
		t.Fatal("force_reload did not rescan the filesystem")
	}

	session := createSessionWithWorkingDirectory(t, handler, workspace)
	withSession := httptest.NewRecorder()
	path := "/skills?session_id=" + session.ID + "&force_reload=true"
	handler.ServeHTTP(withSession, httptest.NewRequest(http.MethodGet, path, nil))
	if withSession.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, withSession.Code, withSession.Body.String())
	}
	var workspaceResult skillsResponse
	if err := decodeAPIData(t, withSession.Body.Bytes(), &workspaceResult); err != nil {
		t.Fatal(err)
	}
	shared := findPublicSkill(workspaceResult.Skills, "shared")
	if shared == nil || shared.Scope != "workspace" || shared.Description != "workspace version" {
		t.Fatalf("workspace override = %#v", shared)
	}
	if len(workspaceResult.Diagnostics) < 2 {
		t.Fatalf("diagnostics = %#v", workspaceResult.Diagnostics)
	}
	if strings.Contains(withSession.Body.String(), home) || strings.Contains(withSession.Body.String(), workspace) {
		t.Fatalf("skills response leaked an absolute path: %s", withSession.Body.String())
	}
}

func TestSessionMessageValidatesAndPersistsSelectedSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	writeDaemonTestSkill(t, filepath.Join(workspace, ".agents", "skills"), "review-taste", "review", "review carefully")
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	session := createSessionWithWorkingDirectory(t, handler, workspace)
	startSession(t, handler, session.ID)
	worker := dialWorker(t, server, session.ID)
	defer worker.Close()
	requests := serveTestWorkerInputs(t, worker, source, session.ID)

	recorder := httptest.NewRecorder()
	body := `{"text":"review this","skills":["review-taste","review-taste"]}`
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/messages", strings.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("message = %d: %s", recorder.Code, recorder.Body.String())
	}
	request := <-requests
	if request.Command.Message == nil || len(request.Command.Message.Skills) != 1 || request.Command.Message.Skills[0] != "review-taste" {
		t.Fatalf("worker input = %#v", request)
	}
	replayed := getCommands(t, handler, "/internal/sessions/"+session.ID+"/commands?after=0")
	if len(replayed.Commands) != 1 || len(replayed.Commands[0].Message.Skills) != 1 || replayed.Commands[0].Message.Skills[0] != "review-taste" {
		t.Fatalf("replayed commands = %#v", replayed)
	}
}

func TestSessionMessageRejectsUnknownSkillAndPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	session := createSession(t, handler)

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/messages", strings.NewReader(`{"text":"review","skills":["missing"]}`)))
	assertAPIError(t, unknown, http.StatusBadRequest, "skill_not_found", "missing")

	pathRequest := httptest.NewRecorder()
	handler.ServeHTTP(pathRequest, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/messages", strings.NewReader(`{"text":"review","path":"/etc/passwd"}`)))
	assertAPIError(t, pathRequest, http.StatusBadRequest, "invalid_request", "path is not accepted")
}

func TestSessionMessageReloadsSkillsBeforeValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".agents", "skills")
	path := writeDaemonTestSkill(t, root, "review-taste", "review", "review carefully")
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	session := createSession(t, handler)

	prime := httptest.NewRecorder()
	handler.ServeHTTP(prime, httptest.NewRequest(http.MethodGet, "/skills", nil))
	if prime.Code != http.StatusOK {
		t.Fatalf("prime skills cache = %d: %s", prime.Code, prime.Body.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/messages", strings.NewReader(`{"text":"review","skills":["review-taste"]}`)))
	assertAPIError(t, recorder, http.StatusBadRequest, "skill_invalid", "review-taste")
}

func TestSkillsAPIRejectsUnsafeSessionID(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/skills?session_id=../registry", nil))
	assertAPIError(t, recorder, http.StatusBadRequest, "invalid_request", "invalid session_id")
}

func TestSkillsAPIDoesNotExposeStoreErrors(t *testing.T) {
	secretPath := "/Users/example/.zotigo/sessions/secret.json"
	store := unavailableSessionStore{err: &os.PathError{Op: "open", Path: secretPath, Err: os.ErrPermission}}
	handler := newHandler(newSessionRegistry(), nil, handlerOptions{store: store})

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/skills?session_id=sess_safe", nil),
		httptest.NewRequest(http.MethodPost, "/sessions/sess_safe/messages", strings.NewReader(`{"text":"review","skills":["review-taste"]}`)),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("%s %s = %d: %s", request.Method, request.URL.Path, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), secretPath) {
			t.Fatalf("response leaked absolute path: %s", recorder.Body.String())
		}
	}
}

func TestNativeRuntimeInjectsCompleteSelectedSkill(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	path := writeDaemonTestSkill(t, root, "review-taste", "review", "review carefully")
	manager := skills.NewSkillManager("", skills.WithUserDir(root))
	message, err := messageFromCommandWithSkills("command-1", &messageCommandPayload{
		Text: "review this", Skills: []string{"review-taste"},
	}, manager)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := message.Content[0].Text
	if !strings.Contains(text, string(raw)) || !strings.HasSuffix(text, "review this") {
		t.Fatalf("native message did not include complete skill: %q", text)
	}
	if got := message.DisplayString(); got != "review this" {
		t.Fatalf("display text leaked injected skill: %q", got)
	}
	if got := sessionadapter.LastUserPrompt([]protocol.Message{message}); got != "review this" {
		t.Fatalf("last prompt leaked injected skill: %q", got)
	}
}

func TestCodexRuntimeBuildsTextAndStructuredSkillInputs(t *testing.T) {
	workspace := t.TempDir()
	path := writeDaemonTestSkill(t, filepath.Join(workspace, ".agents", "skills"), "review-taste", "review", "review carefully")
	runtime := &codexWorkerRuntime{cfg: codexWorkerConfig{WorkingDirectory: workspace}}
	inputs, err := runtime.codexInputs("review this", nil, []string{"review-taste"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0]["type"] != "text" || inputs[0]["text"] != "$review-taste review this" {
		t.Fatalf("Codex inputs = %#v", inputs)
	}
	if inputs[1]["type"] != "skill" || inputs[1]["name"] != "review-taste" || inputs[1]["path"] != path {
		t.Fatalf("Codex skill input = %#v", inputs[1])
	}
	inputs, err = runtime.codexInputs("$review-taste review this", nil, []string{"review-taste"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(inputs[0]["text"].(string), "$review-taste") != 1 {
		t.Fatalf("Codex mention duplicated: %#v", inputs[0])
	}
}

func TestCodexRuntimeMaterializesBuiltinSkill(t *testing.T) {
	runtime := &codexWorkerRuntime{cfg: codexWorkerConfig{WorkingDirectory: t.TempDir()}}
	inputs, err := runtime.codexInputs("create a skill", nil, []string{"skill-creator"})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := inputs[1]["path"].(string)
	if !filepath.IsAbs(path) || filepath.Base(path) != "SKILL.md" {
		t.Fatalf("builtin skill path = %q", path)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || !strings.Contains(string(data), "name: skill-creator") {
		t.Fatalf("materialized builtin = %q, err=%v", data, readErr)
	}
	root := runtime.skillTempDir
	if err := runtime.cleanupSkillFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("builtin skill temp dir was not removed: %v", err)
	}
}

func findPublicSkill(all []publicSkill, name string) *publicSkill {
	for index := range all {
		if all[index].Name == name {
			return &all[index]
		}
	}
	return nil
}

func writeDaemonTestSkill(t *testing.T, root, name, description, instructions string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + instructions + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
