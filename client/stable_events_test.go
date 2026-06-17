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

func TestNormalizeInboundEventUsesDefaultBitbucketMappings(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	event, normalized, err := NormalizeInboundEvent(ctx, store, protocol.EventMsg{
		DeliveryID:     "delivery-bitbucket-1",
		Provider:       "bitbucket",
		GitHubEvent:    "repo.push",
		ExternalEvent:  "repo.push",
		ExternalAction: "tag",
		Repo:           "nitecon/eventic",
		Ref:            "refs/tags/v1.2.3",
		Payload:        json.RawMessage(`{"push":{"changes":[{"new":{"type":"tag","name":"v1.2.3"}}]}}`),
	})
	if err != nil {
		t.Fatalf("normalize bitbucket event: %v", err)
	}
	if event.StableEvent != StableEventArtifactPublished {
		t.Fatalf("expected %s, got %q", StableEventArtifactPublished, event.StableEvent)
	}
	if normalized.MappingStatus != "matched" || normalized.MappingID == "" {
		t.Fatalf("expected matched mapping trace, got %#v", normalized)
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

func TestInboundEventAuditRoundTrip(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	event := protocol.EventMsg{
		DeliveryID:     "audit-1",
		Provider:       "prometheus",
		GitHubEvent:    "alert",
		ExternalEvent:  "alert",
		ExternalAction: "firing",
		Repo:           "nitecon/eventic",
		Ref:            "refs/heads/main",
		Sender:         "HighErrorRate",
		Message:        "error rate high",
		Metadata:       map[string]string{"severity": "CRITICAL", "cluster": "prod"},
		Payload:        json.RawMessage(`{"status":"firing"}`),
	}
	event, normalized, err := NormalizeInboundEvent(ctx, store, event)
	if err != nil {
		t.Fatalf("normalize audit event: %v", err)
	}
	if err := store.RecordInboundEvent(ctx, event, normalized); err != nil {
		t.Fatalf("record inbound event: %v", err)
	}
	if err := store.FinishInboundEvent(ctx, event.DeliveryID, "failure", "node failed"); err != nil {
		t.Fatalf("finish inbound event: %v", err)
	}

	records, err := store.ListInboundEvents(ctx, InboundEventFilter{Repo: "nitecon/eventic", Limit: 10})
	if err != nil {
		t.Fatalf("list inbound events: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	record := records[0]
	if record.StableEvent != StableEventSystemFailure || record.MappingStatus != "matched" {
		t.Fatalf("unexpected mapping audit: %#v", record)
	}
	if record.State != "failure" || record.Description != "node failed" || record.Severity != "CRITICAL" {
		t.Fatalf("unexpected audit state: %#v", record)
	}

	replay := eventFromInboundRecord(record, "replay-1")
	if replay.DeliveryID != "replay-1" || replay.StableEvent != StableEventSystemFailure || replay.Repo != "nitecon/eventic" {
		t.Fatalf("unexpected replay event: %#v", replay)
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
