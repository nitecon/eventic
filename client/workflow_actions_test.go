package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/nitecon/eventic/protocol"
)

func TestTypedHTTPRequestCapturesResponsePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("X-Eventic"); got != "yes" {
			t.Fatalf("expected propagated header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"token":"abc123","items":[{"id":1},{"id":2}]}`))
	}))
	defer srv.Close()

	node, msg := workflowNodeFromStepRequest(workflowStepRequest{
		Name:            "Fetch",
		ActionType:      WorkflowActionHTTPRequest,
		HTTPMethod:      http.MethodPost,
		HTTPURL:         srv.URL,
		HTTPHeaders:     `{"X-Eventic":"yes"}`,
		ResultStatuses:  "202",
		ResponseMode:    ResponseModeSendData,
		ResponsePath:    "body.token",
		ResponseCapture: "TOKEN",
	}, 1, nil)
	if msg != "" {
		t.Fatalf("build node: %s", msg)
	}
	graph := graphFrom(t, []WorkflowNode{node}, nil)
	runner := newNodeRunner(t.TempDir(), protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
	}, nil)
	rc := &RunContext{Vars: map[string]string{}}

	results, err := Execute(context.Background(), graph, rc, runner, 1)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := resultState(results, node.NodeKey); got != NodeStateSuccess {
		t.Fatalf("expected http node success, got %q (%#v)", got, results)
	}
	if rc.Vars["TOKEN"] != "abc123" {
		t.Fatalf("captured TOKEN = %q, want abc123", rc.Vars["TOKEN"])
	}
}

func TestTypedHTTPRequestRetriesTransientFailure(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`try again`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	node, msg := workflowNodeFromStepRequest(workflowStepRequest{
		Name:           "Retry HTTP",
		ActionType:     WorkflowActionHTTPRequest,
		HTTPMethod:     http.MethodGet,
		HTTPURL:        srv.URL,
		ResultStatuses: "200",
		RetryAttempts:  2,
	}, 1, nil)
	if msg != "" {
		t.Fatalf("build node: %s", msg)
	}
	graph := graphFrom(t, []WorkflowNode{node}, nil)
	runner := newNodeRunner(t.TempDir(), protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
	}, nil)

	results, err := Execute(context.Background(), graph, &RunContext{Vars: map[string]string{}}, runner, 1)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := resultState(results, node.NodeKey); got != NodeStateSuccess {
		t.Fatalf("expected retried http node success, got %q (%#v)", got, results)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestProjectMessageActionDispatchesCommsEvent(t *testing.T) {
	node, msg := workflowNodeFromStepRequest(workflowStepRequest{
		Name:           "Notify",
		ActionType:     WorkflowActionProjectMessage,
		ProjectRepo:    "nitecon/another",
		ProjectRef:     "refs/heads/main",
		ProjectMessage: "Deploy ${VERSION}",
	}, 1, nil)
	if msg != "" {
		t.Fatalf("build node: %s", msg)
	}
	graph := graphFrom(t, []WorkflowNode{node}, nil)
	events := make(chan protocol.EventMsg, 1)
	runner := newNodeRunner(t.TempDir(), protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
		Ref:         "refs/heads/main",
	}, func(_ context.Context, event protocol.EventMsg) {
		events <- event
	})

	results, err := Execute(context.Background(), graph, &RunContext{Vars: map[string]string{"VERSION": "v1"}}, runner, 1)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := resultState(results, node.NodeKey); got != NodeStateSuccess {
		t.Fatalf("expected project message node success, got %q (%#v)", got, results)
	}
	event := <-events
	if event.GitHubEvent != "comms" || event.Repo != "nitecon/another" {
		t.Fatalf("unexpected dispatched event: %#v", event)
	}
	if event.Message != "Deploy v1" {
		t.Fatalf("unexpected dispatched message: %q", event.Message)
	}
}

func TestIterateDataRunsPostActionPerItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[{"name":"alpha"},{"name":"beta"}]}`))
	}))
	defer srv.Close()

	node, msg := workflowNodeFromStepRequest(workflowStepRequest{
		Name:            "Fetch items",
		ActionType:      WorkflowActionHTTPRequest,
		HTTPMethod:      http.MethodGet,
		HTTPURL:         srv.URL,
		ResultStatuses:  "200",
		ResponseMode:    ResponseModeIterateData,
		ResponsePath:    "body.items",
		PostActionType:  WorkflowActionProjectMessage,
		PostProjectRepo: "nitecon/target",
		PostProjectMsg:  "Item ${EVENTIC_ITEM_INDEX}: ${EVENTIC_ITEM}",
	}, 1, nil)
	if msg != "" {
		t.Fatalf("build node: %s", msg)
	}

	graph := graphFrom(t, []WorkflowNode{node}, nil)
	events := make(chan protocol.EventMsg, 2)
	runner := newNodeRunner(t.TempDir(), protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
	}, func(_ context.Context, event protocol.EventMsg) {
		events <- event
	})

	results, err := Execute(context.Background(), graph, &RunContext{Vars: map[string]string{}}, runner, 1)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := resultState(results, node.NodeKey); got != NodeStateSuccess {
		t.Fatalf("expected iterate node success, got %q (%#v)", got, results)
	}

	first := <-events
	second := <-events
	if first.Message != `Item 0: {"name":"alpha"}` {
		t.Fatalf("unexpected first message: %q", first.Message)
	}
	if second.Message != `Item 1: {"name":"beta"}` {
		t.Fatalf("unexpected second message: %q", second.Message)
	}
}
