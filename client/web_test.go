package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	store.UpdateOutput(ctx, "nitecon/eventic", "success", "build ok")

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
	if len(project.RecentEvents) != 1 {
		t.Fatalf("expected 1 recent event, got %d", len(project.RecentEvents))
	}
	if project.RecentEvents[0].LatestOutput != "build ok" {
		t.Fatalf("unexpected recent event output: %q", project.RecentEvents[0].LatestOutput)
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

func testTime() time.Time {
	return time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC)
}
