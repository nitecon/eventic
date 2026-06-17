package client

import (
	"encoding/json"
	"testing"

	"github.com/nitecon/eventic/protocol"
)

func TestNormalizeInboundEventUsesDefaultGitHubMappings(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	payload := json.RawMessage(`{"after":"abc123"}`)
	event, normalized, err := NormalizeInboundEvent(ctx, store, protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
		Ref:         "refs/heads/main",
		Sender:      "nitecon",
		Payload:     payload,
	})
	if err != nil {
		t.Fatalf("normalize event: %v", err)
	}
	if event.StableEvent != StableEventArtifactInitiate {
		t.Fatalf("expected %s, got %q", StableEventArtifactInitiate, event.StableEvent)
	}
	if normalized.Source != "github" || normalized.StableEvent != StableEventArtifactInitiate {
		t.Fatalf("unexpected normalized event: %#v", normalized)
	}
	if event.Metadata["commit_sha"] != "abc123" {
		t.Fatalf("expected commit metadata, got %#v", event.Metadata)
	}
}

func TestNormalizeInboundEventCanResolveWorkflowByStableEvent(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	if _, err := store.CreateWorkflow(ctx, &Workflow{
		Scope:     WorkflowScopeGlobal,
		EventType: StableEventArtifactInitiate,
		Name:      "build",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	event, _, err := NormalizeInboundEvent(ctx, store, protocol.EventMsg{
		DeliveryID:  "delivery-2",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
		Ref:         "refs/heads/main",
		Payload:     json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("normalize event: %v", err)
	}

	wf, err := store.ResolveWorkflow(ctx, event.Repo, workflowEventType(event), workflowEventAction(event))
	if err != nil {
		t.Fatalf("resolve workflow: %v", err)
	}
	if wf == nil || wf.Name != "build" {
		t.Fatalf("expected stable workflow, got %#v", wf)
	}
}

func TestEventMappingMatchesBodyArrayCondition(t *testing.T) {
	event := protocol.EventMsg{
		Provider:    "custom",
		GitHubEvent: "scan",
		Payload:     json.RawMessage(`{"vulnerabilities":[{"id":"CVE-1","severity":"LOW"},{"id":"CVE-2","severity":"CRITICAL"}]}`),
	}
	mapping := EventMapping{
		Provider:          "custom",
		Enabled:           true,
		Conditions:        map[string]string{"body.vulnerabilities[].severity": "CRITICAL"},
		TargetStableEvent: StableEventSecurityAlarm,
	}

	if !mappingMatchesEvent(mapping, event) {
		t.Fatal("expected array condition to match")
	}
}
