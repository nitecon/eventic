package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/eventic/protocol"
)

func TestExecutionLogTrimsEventsAndOutput(t *testing.T) {
	logStore := NewExecutionLog(WebConfig{
		MaxEvents:      2,
		MaxOutputBytes: 8,
	})

	for _, id := range []string{"one", "two", "three"} {
		logStore.StartEvent(protocol.EventMsg{
			DeliveryID:  id,
			GitHubEvent: "push",
			Repo:        "nitecon/eventic",
		})
	}

	logStore.StartHook("three", "global:post")
	logStore.FinishHook("three", "global:post", "success", "0123456789abcdef")
	logStore.FinishEvent("three", "success", "")

	events := logStore.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].DeliveryID != "three" || events[1].DeliveryID != "two" {
		t.Fatalf("unexpected event order: %#v", events)
	}
	if events[0].Hooks[0].Output != "01234567" {
		t.Fatalf("expected output truncation, got %q", events[0].Hooks[0].Output)
	}
}

func TestEventsHandlerReturnsJSON(t *testing.T) {
	logStore := NewExecutionLog(WebConfig{})
	logStore.StartEvent(protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "workflow_run",
		Action:      "completed",
		Repo:        "nitecon/eventic",
	})
	logStore.FinishEvent("delivery-1", "success", "ok")

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()

	eventsHandler(logStore).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var events []ExecutionEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("expected json response: %v", err)
	}
	if len(events) != 1 || events[0].DeliveryID != "delivery-1" {
		t.Fatalf("unexpected events response: %#v", events)
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("expected json content type, got %q", contentType)
	}
}

func TestWebIndexIncludesActiveAndExistingProjectSections(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	webIndexHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, expected := range []string{
		"Active Projects",
		"Existing Projects",
		`id="active-projects"`,
		`id="existing-projects"`,
		`id="project-search"`,
		"Event Queue",
		`id="event-queue"`,
		`fetch("/projects")`,
		`function refreshSnapshot()`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected index body to contain %q", expected)
		}
	}
}

func TestProjectsHandlerReturnsProjectByRepo(t *testing.T) {
	ctx := t.Context()
	store, err := OpenProjectStore(ctx, StateConfig{
		Enabled: true,
		Path:    t.TempDir() + "/eventic.db",
	})
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	defer store.Close()

	event := ExecutionEvent{
		DeliveryID: "delivery-1",
		Repo:       "nitecon/eventic",
		Event:      "push",
		State:      "running",
		StartedAt:  testTime(),
		UpdatedAt:  testTime(),
	}
	store.StartProject(ctx, event)
	store.UpdateGitState(ctx, "nitecon/eventic", "main", "abc123")
	var cfg Config
	cfg.GlobalHooks.Post = "echo ok"
	store.SyncConfiguredEvents(ctx, "nitecon/eventic", "", cfg)
	store.UpdateOutput(ctx, "nitecon/eventic", protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
	}, "global:post", "success", "build ok")

	req := httptest.NewRequest(http.MethodGet, "/projects/nitecon/eventic", nil)
	rec := httptest.NewRecorder()

	projectsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var project ProjectState
	if err := json.Unmarshal(rec.Body.Bytes(), &project); err != nil {
		t.Fatalf("expected json response: %v", err)
	}
	if project.Repo != "nitecon/eventic" {
		t.Fatalf("unexpected project repo: %q", project.Repo)
	}
	if project.Hash != "abc123" {
		t.Fatalf("unexpected hash: %q", project.Hash)
	}
	if project.LatestOutput != "build ok" {
		t.Fatalf("unexpected latest output: %q", project.LatestOutput)
	}
	if len(project.RecentEvents) != 0 {
		t.Fatalf("expected project detail to omit recent raw events, got %d", len(project.RecentEvents))
	}
	if len(project.ConfiguredEvents) != 1 {
		t.Fatalf("expected 1 configured event, got %d", len(project.ConfiguredEvents))
	}
	if project.ConfiguredEvents[0].EventKey != "global:post" {
		t.Fatalf("unexpected configured event key: %q", project.ConfiguredEvents[0].EventKey)
	}
	if project.ConfiguredEvents[0].LatestOutput != "build ok" {
		t.Fatalf("unexpected configured event output: %q", project.ConfiguredEvents[0].LatestOutput)
	}
}

func TestProjectsHandlerReturnsProjectNameList(t *testing.T) {
	ctx := t.Context()
	store, err := OpenProjectStore(ctx, StateConfig{
		Enabled: true,
		Path:    t.TempDir() + "/eventic.db",
	})
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	defer store.Close()

	store.UpsertManagedProject(ctx, "nitecon/eventic", "main", "abc123")
	store.UpsertManagedProject(ctx, "nitecon/another", "main", "def456")
	var cfg Config
	cfg.GlobalHooks.Post = "echo configured"
	store.SyncConfiguredEvents(ctx, "nitecon/eventic", "", cfg)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	rec := httptest.NewRecorder()

	projectsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var projects []string
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("expected json string array response: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	seen := map[string]bool{}
	for _, repo := range projects {
		seen[repo] = true
	}
	for _, repo := range []string{"nitecon/eventic", "nitecon/another"} {
		if !seen[repo] {
			t.Fatalf("expected project %q in %#v", repo, projects)
		}
	}
}

func TestProjectStoreRetainsLastFiveEventsPerRepo(t *testing.T) {
	ctx := t.Context()
	store, err := OpenProjectStore(ctx, StateConfig{
		Enabled: true,
		Path:    t.TempDir() + "/eventic.db",
	})
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	defer store.Close()

	for i := 0; i < 7; i++ {
		event := ExecutionEvent{
			DeliveryID:  fmt.Sprintf("delivery-%d", i),
			Repo:        "nitecon/eventic",
			Event:       "push",
			State:       "success",
			Description: "ok",
			StartedAt:   testTime().Add(time.Duration(i) * time.Minute),
			UpdatedAt:   testTime().Add(time.Duration(i) * time.Minute),
			Hooks: []HookExecution{{
				Name:   "global:post",
				State:  "success",
				Output: fmt.Sprintf("output-%d", i),
			}},
		}
		store.StartProject(ctx, event)
		store.FinishProject(ctx, event)
	}

	events, err := store.ListProjectEvents(ctx, "nitecon/eventic", 10)
	if err != nil {
		t.Fatalf("list project events: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 retained events, got %d", len(events))
	}
	if events[0].DeliveryID != "delivery-6" {
		t.Fatalf("expected newest event first, got %q", events[0].DeliveryID)
	}
	if events[4].DeliveryID != "delivery-2" {
		t.Fatalf("expected oldest retained event delivery-2, got %q", events[4].DeliveryID)
	}
}

func TestSeedFromReposDirAddsDiskReposAndConfiguredEvents(t *testing.T) {
	ctx := t.Context()
	reposDir := t.TempDir()
	repoPath := filepath.Join(reposDir, "nitecon", "eventic")
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	writeFile(t, filepath.Join(repoPath, "README.md"), "# test\n")
	writeFile(t, filepath.Join(repoPath, ".eventic.yaml"), `
hooks:
  pre: "echo repo pre"
events:
  push:
    post: "echo push post"
`)
	runGit(t, repoPath, "init")
	runGit(t, repoPath, "config", "user.email", "eventic@example.com")
	runGit(t, repoPath, "config", "user.name", "Eventic Test")
	runGit(t, repoPath, "add", ".")
	runGit(t, repoPath, "commit", "-m", "init")

	store, err := OpenProjectStore(ctx, StateConfig{
		Enabled: true,
		Path:    t.TempDir() + "/eventic.db",
	})
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	defer store.Close()

	var cfg Config
	cfg.GlobalHooks.Post = "echo global post"
	store.SeedFromReposDir(ctx, reposDir, cfg)

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list seeded projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 seeded project, got %d", len(projects))
	}
	if projects[0] != "nitecon/eventic" {
		t.Fatalf("unexpected seeded project repo: %q", projects[0])
	}

	project, err := store.GetProject(ctx, "nitecon/eventic")
	if err != nil {
		t.Fatalf("get seeded project: %v", err)
	}
	if project.Hash == "" {
		t.Fatal("expected seeded project hash")
	}

	keys := map[string]bool{}
	for _, event := range project.ConfiguredEvents {
		keys[event.EventKey] = true
	}
	for _, key := range []string{"global:post", "global:pre", "push:post"} {
		if !keys[key] {
			t.Fatalf("expected configured event %q in %#v", key, project.ConfiguredEvents)
		}
	}
}

func testTime() time.Time {
	return time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
