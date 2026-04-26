package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
