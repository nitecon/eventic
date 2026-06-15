package client

import (
	"testing"
)

// newTestStore opens an isolated in-memory project store for a single test.
func newTestStore(t *testing.T) *ProjectStore {
	t.Helper()
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory project store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestWorkflowCRUDRoundTrip(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	wf := &Workflow{
		Scope:     WorkflowScopeRepo,
		Repo:      "nitecon/eventic",
		EventType: "push",
		Name:      "deploy",
		Enabled:   true,
		Nodes: []WorkflowNode{
			{NodeKey: "rag", Name: "RAG fetch", Command: "echo rag", Capture: "CONTEXT", PosX: 1, PosY: 2},
			{NodeKey: "assess", Name: "Assess", Command: "echo assess", Capture: "VERDICT", ContinueOnError: true, TimeoutSeconds: 30},
		},
		Edges: []WorkflowEdge{
			{FromNode: "rag", ToNode: "assess", Condition: "success"},
		},
	}

	id, err := store.CreateWorkflow(ctx, wf)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero workflow id")
	}

	got, err := store.GetWorkflow(ctx, id)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if got.Name != "deploy" || got.Scope != WorkflowScopeRepo || got.EventType != "push" {
		t.Fatalf("unexpected workflow metadata: %#v", got)
	}
	if got.Version != 1 {
		t.Fatalf("expected version 1, got %d", got.Version)
	}
	if len(got.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(got.Nodes))
	}
	if len(got.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(got.Edges))
	}
	if !got.Nodes[1].ContinueOnError || got.Nodes[1].Capture != "VERDICT" || got.Nodes[1].TimeoutSeconds != 30 {
		t.Fatalf("node fields not round-tripped: %#v", got.Nodes[1])
	}
	if got.Edges[0].FromNode != "rag" || got.Edges[0].ToNode != "assess" || got.Edges[0].Condition != "success" {
		t.Fatalf("edge not round-tripped: %#v", got.Edges[0])
	}

	list, err := store.ListWorkflows(ctx, WorkflowScopeRepo, "nitecon/eventic", "")
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 workflow in list, got %d", len(list))
	}
	if len(list[0].Nodes) != 0 {
		t.Fatalf("list summaries should omit nodes, got %d", len(list[0].Nodes))
	}

	if err := store.DeleteWorkflow(ctx, id); err != nil {
		t.Fatalf("delete workflow: %v", err)
	}
	if _, err := store.GetWorkflow(ctx, id); err == nil {
		t.Fatal("expected error fetching deleted workflow")
	}
}

func TestUpdateWorkflowReplacesNodesAndEdges(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	id, err := store.CreateWorkflow(ctx, &Workflow{
		Scope:     WorkflowScopeRepo,
		Repo:      "nitecon/eventic",
		EventType: "push",
		Name:      "initial",
		Enabled:   true,
		Nodes: []WorkflowNode{
			{NodeKey: "a", Command: "echo a"},
			{NodeKey: "b", Command: "echo b"},
		},
		Edges: []WorkflowEdge{{FromNode: "a", ToNode: "b", Condition: "always"}},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	wf, err := store.GetWorkflow(ctx, id)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}

	wf.Name = "updated"
	wf.Nodes = []WorkflowNode{{NodeKey: "c", Command: "echo c"}}
	wf.Edges = nil
	if err := store.UpdateWorkflow(ctx, wf); err != nil {
		t.Fatalf("update workflow: %v", err)
	}

	got, err := store.GetWorkflow(ctx, id)
	if err != nil {
		t.Fatalf("get updated workflow: %v", err)
	}
	if got.Name != "updated" {
		t.Fatalf("expected updated name, got %q", got.Name)
	}
	if got.Version != 2 {
		t.Fatalf("expected version bumped to 2, got %d", got.Version)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].NodeKey != "c" {
		t.Fatalf("expected nodes replaced with [c], got %#v", got.Nodes)
	}
	if len(got.Edges) != 0 {
		t.Fatalf("expected edges cleared, got %d", len(got.Edges))
	}
}

func TestResolveWorkflowPrecedence(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	mustCreate := func(scope, repo, eventType, name string, enabled bool) {
		if _, err := store.CreateWorkflow(ctx, &Workflow{
			Scope:     scope,
			Repo:      repo,
			EventType: eventType,
			Name:      name,
			Enabled:   enabled,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	mustCreate(WorkflowScopeGlobal, "", "pull_request", "global-generic", true)
	mustCreate(WorkflowScopeGlobal, "", "pull_request.opened", "global-action", true)
	mustCreate(WorkflowScopeRepo, "nitecon/eventic", "pull_request", "repo-generic", true)
	mustCreate(WorkflowScopeRepo, "nitecon/eventic", "pull_request.opened", "repo-action", true)

	// Most specific wins: repo + action.
	wf, err := store.ResolveWorkflow(ctx, "nitecon/eventic", "pull_request", "opened")
	if err != nil {
		t.Fatalf("resolve repo-action: %v", err)
	}
	if wf == nil || wf.Name != "repo-action" {
		t.Fatalf("expected repo-action, got %#v", wf)
	}

	// Repo generic beats global when no action-specific repo workflow exists.
	wf, err = store.ResolveWorkflow(ctx, "nitecon/eventic", "pull_request", "closed")
	if err != nil {
		t.Fatalf("resolve repo-generic: %v", err)
	}
	if wf == nil || wf.Name != "repo-generic" {
		t.Fatalf("expected repo-generic, got %#v", wf)
	}

	// A different repo with no workflows falls back to global action-specific.
	wf, err = store.ResolveWorkflow(ctx, "nitecon/other", "pull_request", "opened")
	if err != nil {
		t.Fatalf("resolve global-action: %v", err)
	}
	if wf == nil || wf.Name != "global-action" {
		t.Fatalf("expected global-action, got %#v", wf)
	}

	// A different repo, generic event falls back to global generic.
	wf, err = store.ResolveWorkflow(ctx, "nitecon/other", "pull_request", "closed")
	if err != nil {
		t.Fatalf("resolve global-generic: %v", err)
	}
	if wf == nil || wf.Name != "global-generic" {
		t.Fatalf("expected global-generic, got %#v", wf)
	}

	// No match at all returns (nil, nil).
	wf, err = store.ResolveWorkflow(ctx, "nitecon/other", "push", "")
	if err != nil {
		t.Fatalf("resolve no-match: %v", err)
	}
	if wf != nil {
		t.Fatalf("expected nil workflow for no match, got %#v", wf)
	}
}

func TestResolveWorkflowExcludesDisabled(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	// Disabled repo-specific workflow must be skipped in favour of enabled global.
	if _, err := store.CreateWorkflow(ctx, &Workflow{
		Scope: WorkflowScopeRepo, Repo: "nitecon/eventic", EventType: "push",
		Name: "repo-disabled", Enabled: false,
	}); err != nil {
		t.Fatalf("create repo-disabled: %v", err)
	}
	if _, err := store.CreateWorkflow(ctx, &Workflow{
		Scope: WorkflowScopeGlobal, Repo: "", EventType: "push",
		Name: "global-enabled", Enabled: true,
	}); err != nil {
		t.Fatalf("create global-enabled: %v", err)
	}

	wf, err := store.ResolveWorkflow(ctx, "nitecon/eventic", "push", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if wf == nil || wf.Name != "global-enabled" {
		t.Fatalf("expected disabled repo workflow to be skipped for global-enabled, got %#v", wf)
	}

	// When only a disabled workflow exists for an event, resolution returns nil.
	if _, err := store.CreateWorkflow(ctx, &Workflow{
		Scope: WorkflowScopeRepo, Repo: "nitecon/eventic", EventType: "release",
		Name: "only-disabled", Enabled: false,
	}); err != nil {
		t.Fatalf("create only-disabled: %v", err)
	}
	wf, err = store.ResolveWorkflow(ctx, "nitecon/eventic", "release", "")
	if err != nil {
		t.Fatalf("resolve only-disabled: %v", err)
	}
	if wf != nil {
		t.Fatalf("expected nil for only-disabled, got %#v", wf)
	}
}

func TestWorkflowRunsLifecycle(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)

	wfID, err := store.CreateWorkflow(ctx, &Workflow{
		Scope: WorkflowScopeRepo, Repo: "nitecon/eventic", EventType: "push",
		Name: "run-wf", Enabled: true,
		Nodes: []WorkflowNode{{NodeKey: "a", Command: "echo a"}},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	runID, err := store.StartRun(ctx, &WorkflowRun{
		WorkflowID: wfID,
		Repo:       "nitecon/eventic",
		EventType:  "push",
		DeliveryID: "comms-123",
		Ref:        "main",
		Trigger:    "comms",
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	store.StartRunNode(ctx, runID, "a")
	store.FinishRunNode(ctx, runID, "a", "success", 0, "hello output")
	store.FinishRun(ctx, runID, "success")

	run, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != "success" || run.Trigger != "comms" {
		t.Fatalf("unexpected run: %#v", run)
	}
	if len(run.Nodes) != 1 {
		t.Fatalf("expected 1 run node, got %d", len(run.Nodes))
	}
	if run.Nodes[0].State != "success" || run.Nodes[0].Output != "hello output" {
		t.Fatalf("unexpected run node: %#v", run.Nodes[0])
	}

	runs, err := store.ListRuns(ctx, "nitecon/eventic", 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
}
