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

func TestEventMappingMatchesWildcardRefCondition(t *testing.T) {
	event := protocol.EventMsg{
		Provider:      "github",
		GitHubEvent:   "push",
		ExternalEvent: "push",
		Ref:           "refs/tags/v1.2.3",
		Payload:       json.RawMessage(`{}`),
	}
	mapping := EventMapping{
		Provider:          "github",
		Enabled:           true,
		Conditions:        map[string]string{"event": "push", "ref": "refs/tags/*"},
		TargetStableEvent: StableEventArtifactPublished,
	}

	if !mappingMatchesEvent(mapping, event) {
		t.Fatal("expected wildcard ref condition to match")
	}
}

func TestStableEventRegistrySupportsCustomEvents(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	stableEvents, err := store.ListStableEvents(ctx)
	if err != nil {
		t.Fatalf("list seeded stable events: %v", err)
	}
	if len(stableEvents) < len(StableEventDefinitions()) {
		t.Fatalf("expected seeded stable events, got %d", len(stableEvents))
	}

	id, err := store.CreateStableEvent(ctx, &StableEventDefinition{
		Event:       "catchall",
		Title:       "Catch All",
		Group:       "Custom",
		Description: "Route any unmatched provider event.",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("create custom stable event: %v", err)
	}
	stableEvent, err := store.GetStableEvent(ctx, id)
	if err != nil {
		t.Fatalf("get custom stable event: %v", err)
	}
	if stableEvent.Event != "catchall" || stableEvent.BuiltIn {
		t.Fatalf("unexpected custom stable event: %#v", stableEvent)
	}

	stableEvent.Event = "catchall.renamed"
	if err := store.UpdateStableEvent(ctx, stableEvent); err != nil {
		t.Fatalf("rename custom stable event: %v", err)
	}
	renamed, err := store.GetStableEvent(ctx, id)
	if err != nil {
		t.Fatalf("get renamed stable event: %v", err)
	}
	if renamed.Event != "catchall.renamed" {
		t.Fatalf("expected renamed stable event, got %#v", renamed)
	}
}

func TestBuiltInStableEventCannotBeRenamed(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	stableEvents, err := store.ListStableEvents(ctx)
	if err != nil {
		t.Fatalf("list seeded stable events: %v", err)
	}
	if len(stableEvents) == 0 {
		t.Fatal("expected seeded stable events")
	}
	builtIn := stableEvents[0]
	if !builtIn.BuiltIn {
		t.Fatalf("expected built-in event, got %#v", builtIn)
	}
	builtIn.Event = "renamed"
	if err := store.UpdateStableEvent(ctx, &builtIn); err != ErrStableEventBuiltIn {
		t.Fatalf("expected built-in rename error, got %v", err)
	}
}
