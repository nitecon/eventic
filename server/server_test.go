package server

import (
	"net/http"
	"testing"
)

func TestParseGenericWebhookPrometheus(t *testing.T) {
	body := []byte(`{
		"status":"firing",
		"commonLabels":{
			"repo":"nitecon/eventic",
			"ref":"refs/heads/main",
			"severity":"critical",
			"alertname":"HighErrorRate"
		},
		"commonAnnotations":{"summary":"high error rate"}
	}`)
	headers := http.Header{}
	headers.Set("X-Request-Id", "alert-1")

	event, err := parseGenericWebhook("prometheus", headers, body)
	if err != nil {
		t.Fatalf("parse generic webhook: %v", err)
	}
	if event.Provider != "prometheus" || event.GitHubEvent != "alert" || event.Action != "firing" {
		t.Fatalf("unexpected event identity: %#v", event)
	}
	if event.Repo != "nitecon/eventic" || event.Ref != "refs/heads/main" {
		t.Fatalf("unexpected project target: %#v", event)
	}
	if event.Metadata["severity"] != "CRITICAL" {
		t.Fatalf("expected severity metadata, got %#v", event.Metadata)
	}
}

func TestParseStableEventRequest(t *testing.T) {
	event, err := parseStableEventRequest("catchall", []byte(`{
		"delivery_id":"evt-1",
		"repo":"nitecon/eventic",
		"ref":"refs/heads/main",
		"title":"Deploy failed",
		"body":"Smoke tests failed",
		"action":"notify",
		"sender":"sre-bot",
		"metadata":{"severity":"CRITICAL"}
	}`))
	if err != nil {
		t.Fatalf("parse stable event request: %v", err)
	}
	if event.StableEvent != "catchall" || event.Provider != "eventic" || event.GitHubEvent != "internal" {
		t.Fatalf("unexpected event identity: %#v", event)
	}
	if event.Repo != "nitecon/eventic" || event.Ref != "refs/heads/main" {
		t.Fatalf("unexpected project target: %#v", event)
	}
	if event.Message != "Deploy failed\n\nSmoke tests failed" {
		t.Fatalf("unexpected message: %q", event.Message)
	}
	if event.Metadata["severity"] != "CRITICAL" || event.Metadata["title"] != "Deploy failed" {
		t.Fatalf("unexpected metadata: %#v", event.Metadata)
	}
}
