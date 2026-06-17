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

func TestParseGenericWebhookGitLabPipelineFailure(t *testing.T) {
	body := []byte(`{
		"object_kind":"pipeline",
		"object_attributes":{"status":"failed","ref":"main","sha":"abc123"},
		"project":{"path_with_namespace":"nitecon/eventic","git_http_url":"https://gitlab.com/nitecon/eventic.git"},
		"user_username":"deploy-bot"
	}`)
	headers := http.Header{}
	headers.Set("X-Gitlab-Event", "Pipeline Hook")
	headers.Set("X-Gitlab-Event-UUID", "gitlab-1")

	event, err := parseGenericWebhook("gitlab", headers, body)
	if err != nil {
		t.Fatalf("parse gitlab webhook: %v", err)
	}
	if event.Provider != "gitlab" || event.GitHubEvent != "pipeline" || event.Action != "failed" {
		t.Fatalf("unexpected event identity: %#v", event)
	}
	if event.Repo != "nitecon/eventic" || event.Ref != "refs/heads/main" {
		t.Fatalf("unexpected gitlab target: %#v", event)
	}
	if event.Metadata["commit_sha"] != "abc123" {
		t.Fatalf("expected commit metadata, got %#v", event.Metadata)
	}
}

func TestParseGenericWebhookBitbucketTagPush(t *testing.T) {
	body := []byte(`{
		"repository":{"full_name":"nitecon/eventic"},
		"push":{"changes":[{"new":{"type":"tag","name":"v1.2.3","target":{"hash":"abc123"}}}]},
		"actor":{"username":"release-bot"}
	}`)
	headers := http.Header{}
	headers.Set("X-Event-Key", "repo:push")
	headers.Set("X-Request-Id", "bitbucket-1")

	event, err := parseGenericWebhook("bitbucket", headers, body)
	if err != nil {
		t.Fatalf("parse bitbucket webhook: %v", err)
	}
	if event.Provider != "bitbucket" || event.GitHubEvent != "repo.push" {
		t.Fatalf("unexpected event identity: %#v", event)
	}
	if event.Repo != "nitecon/eventic" || event.Ref != "refs/tags/v1.2.3" {
		t.Fatalf("unexpected bitbucket target: %#v", event)
	}
	if event.Metadata["commit_sha"] != "abc123" {
		t.Fatalf("expected bitbucket commit metadata, got %#v", event.Metadata)
	}
}

func TestParseGenericWebhookDiscordCommand(t *testing.T) {
	body := []byte(`{
		"type":2,
		"data":{"name":"deploy","repo":"nitecon/eventic"},
		"member":{"user":{"username":"sre-user"}}
	}`)
	headers := http.Header{}
	headers.Set("X-Request-Id", "discord-1")

	event, err := parseGenericWebhook("discord", headers, body)
	if err != nil {
		t.Fatalf("parse discord webhook: %v", err)
	}
	if event.Provider != "discord" || event.GitHubEvent != "application_command" || event.Action != "deploy" {
		t.Fatalf("unexpected discord event identity: %#v", event)
	}
	if event.Repo != "nitecon/eventic" || event.Sender != "sre-user" {
		t.Fatalf("unexpected discord target: %#v", event)
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
