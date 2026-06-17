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
