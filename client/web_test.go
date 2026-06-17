package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func TestWebMuxRoutePatternsDoNotConflict(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	mux := webMux(Config{}, NewExecutionLog(WebConfig{}), store, func(context.Context, protocol.EventMsg) {})
	for _, path := range []string{"/", "/global", "/api/projects"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("route %s unexpectedly returned 404", path)
		}
	}
}

func TestIndexHandlerServesEmbeddedDashboard(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, expected := range []string{
		`class="app-page"`,
		`name="endpoint:workflows"`,
		`name="endpoint:runs-ws"`,
		"ndesign-cdn/ndesign/latest/",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected shell body to contain %q", expected)
		}
	}
}

func TestIndexHandlerServesStaticDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), "<html><body>custom ndesign shell</body></html>")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	indexHandler(Config{Web: WebConfig{StaticDir: dir}}, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "custom ndesign shell") {
		t.Fatalf("expected static index served, got %q", rec.Body.String())
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
	store.UpdateOutput(ctx, "nitecon/eventic", protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
	}, "global:post", "success", "build ok")

	req := httptest.NewRequest(http.MethodGet, "/api/projects/nitecon/eventic", nil)
	rec := httptest.NewRecorder()

	apiProjectsHandler(store).ServeHTTP(rec, req)

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
	if len(project.RecentEvents) != 0 {
		t.Fatalf("expected project detail to omit recent raw events, got %d", len(project.RecentEvents))
	}
}

func TestReplayRefsHandlerReturnsBranchesAndTags(t *testing.T) {
	reposDir := t.TempDir()
	repoPath := filepath.Join(reposDir, "nitecon", "traderx")
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	writeFile(t, filepath.Join(repoPath, "README.md"), "# traderx\n")
	runGit(t, repoPath, "init")
	runGit(t, repoPath, "config", "user.email", "eventic@example.com")
	runGit(t, repoPath, "config", "user.name", "Eventic Test")
	runGit(t, repoPath, "add", ".")
	runGit(t, repoPath, "commit", "-m", "init")
	runGit(t, repoPath, "branch", "feature/replay")
	runGit(t, repoPath, "tag", "v1.0.0")

	req := httptest.NewRequest(http.MethodGet, "/api/refs?repo=nitecon/traderx", nil)
	rec := httptest.NewRecorder()

	apiRefsHandler(Config{ReposDir: reposDir}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var refs []ReplayRef
	if err := json.Unmarshal(rec.Body.Bytes(), &refs); err != nil {
		t.Fatalf("expected json response: %v", err)
	}
	seen := map[string]string{}
	for _, ref := range refs {
		seen[ref.Ref] = ref.Type
	}
	if seen["refs/heads/feature/replay"] != "branch" {
		t.Fatalf("expected feature branch in refs: %#v", refs)
	}
	if seen["refs/tags/v1.0.0"] != "tag" {
		t.Fatalf("expected tag in refs: %#v", refs)
	}
}

func TestTriggerRunDispatchesEventWithMessage(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	events := make(chan protocol.EventMsg, 1)
	body := bytes.NewBufferString(`{"repo":"nitecon/traderx","event_type":"comms","ref":"refs/heads/main","message":"assess this"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/runs", body)
	rec := httptest.NewRecorder()

	runsCollectionHandler(store, func(ctx context.Context, event protocol.EventMsg) {
		events <- event
	}).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d: %s", rec.Code, rec.Body.String())
	}
	event := <-events
	if event.Repo != "nitecon/traderx" {
		t.Fatalf("unexpected repo: %q", event.Repo)
	}
	if event.GitHubEvent != "comms" || event.Action != "" {
		t.Fatalf("expected comms event, got %s.%s", event.GitHubEvent, event.Action)
	}
	if event.Ref != "refs/heads/main" {
		t.Fatalf("unexpected ref: %q", event.Ref)
	}
	if event.Message != "assess this" {
		t.Fatalf("expected message passthrough, got %q", event.Message)
	}
	if event.Sender != "eventic-web" {
		t.Fatalf("unexpected sender: %q", event.Sender)
	}
	if event.CloneURL != "https://github.com/nitecon/traderx.git" {
		t.Fatalf("unexpected clone url: %q", event.CloneURL)
	}
	if !strings.HasPrefix(event.DeliveryID, "manual-") {
		t.Fatalf("expected manual delivery id, got %q", event.DeliveryID)
	}
}

func TestTriggerRunRejectsMissingFields(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	body := bytes.NewBufferString(`{"repo":"nitecon/traderx"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/runs", body)
	rec := httptest.NewRecorder()

	runsCollectionHandler(store, func(context.Context, protocol.EventMsg) {}).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
	errs := decodeEnvelope(t, rec)
	if errs["event_type"] == "" || errs["ref"] == "" {
		t.Fatalf("expected event_type and ref field errors, got %#v", errs)
	}
}

func TestWorkflowCRUDLifecycle(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	collection := workflowsCollectionHandler(store)
	item := workflowItemHandler(store)

	// Create a valid two-node workflow.
	createBody := `{
		"scope":"repo","repo":"nitecon/eventic","event_type":"push","name":"ci","enabled":true,
		"nodes":[
			{"node_key":"a","name":"step a","type":"command","command":"echo a"},
			{"node_key":"b","name":"step b","type":"command","command":"echo b"}
		],
		"edges":[{"from_node":"a","to_node":"b","condition":"success"}]
	}`
	rec := httptest.NewRecorder()
	collection.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/workflows", strings.NewReader(createBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created Workflow
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("expected created workflow to carry an id")
	}
	if len(created.Nodes) != 2 || len(created.Edges) != 1 {
		t.Fatalf("expected 2 nodes / 1 edge, got %d / %d", len(created.Nodes), len(created.Edges))
	}

	path := fmt.Sprintf("/api/workflows/%d", created.ID)

	// Get the workflow back.
	rec = httptest.NewRecorder()
	item.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rec.Code)
	}

	// List should include it.
	rec = httptest.NewRecorder()
	collection.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflows?scope=repo&repo=nitecon/eventic", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var listed []Workflow
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("expected listed workflow %d, got %#v", created.ID, listed)
	}

	// Update with a cycle → expect a graph-level error envelope.
	cycleBody := `{
		"scope":"repo","repo":"nitecon/eventic","event_type":"push","name":"ci","enabled":true,
		"nodes":[
			{"node_key":"a","name":"a","type":"command","command":"echo a"},
			{"node_key":"b","name":"b","type":"command","command":"echo b"}
		],
		"edges":[
			{"from_node":"a","to_node":"b","condition":"always"},
			{"from_node":"b","to_node":"a","condition":"always"}
		]
	}`
	rec = httptest.NewRecorder()
	item.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, path, strings.NewReader(cycleBody)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cycle update: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	errs := decodeEnvelope(t, rec)
	if !strings.Contains(errs["error"], "cycle") {
		t.Fatalf("expected cycle error, got %#v", errs)
	}

	// Delete the workflow.
	rec = httptest.NewRecorder()
	item.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", rec.Code)
	}

	// Subsequent get should 404 with an envelope.
	rec = httptest.NewRecorder()
	item.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rec.Code)
	}
}

func TestWorkflowConfigLifecycle(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	collection := workflowConfigCollectionHandler(store)

	createBody := `{"name":"ci","event_type":"push","steps":"Lint | make lint\nmake test"}`
	rec := httptest.NewRecorder()
	collection.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/workflow-config?scope=repo&repo=nitecon/eventic", strings.NewReader(createBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create config: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created workflowConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created config: %v", err)
	}
	if created.ID == 0 || created.StepsCount != 2 {
		t.Fatalf("unexpected created config: %#v", created)
	}

	full, err := store.GetWorkflow(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("read created workflow: %v", err)
	}
	if len(full.Nodes) != 2 || len(full.Edges) != 1 {
		t.Fatalf("expected linear 2-node workflow, got nodes=%d edges=%d", len(full.Nodes), len(full.Edges))
	}
	if full.Nodes[0].Name != "Lint" || full.Nodes[0].Command != "make lint" {
		t.Fatalf("unexpected first node: %#v", full.Nodes[0])
	}

	rec = httptest.NewRecorder()
	collection.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflow-config?scope=repo&repo=nitecon/eventic", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list config: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listed []workflowConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed configs: %v", err)
	}
	if len(listed) != 1 || listed[0].StepsCount != 2 {
		t.Fatalf("unexpected config list: %#v", listed)
	}

	updateBody := `{"name":"ci","event_type":"pull_request.opened","steps":"make verify"}`
	rec = httptest.NewRecorder()
	itemPath := fmt.Sprintf("/api/workflow-config/%d", created.ID)
	workflowConfigItemHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, itemPath, strings.NewReader(updateBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update config: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var updated workflowConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated config: %v", err)
	}
	if updated.EventType != "pull_request.opened" || updated.StepsCount != 1 {
		t.Fatalf("unexpected updated config: %#v", updated)
	}
}

func TestWorkflowConfigTypedStepLifecycle(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	collection := workflowConfigCollectionHandler(store)
	createBody := `{
		"name":"notify",
		"event_type":"push",
		"step_name":"Fetch hook",
		"action_type":"send_http_request",
		"http_method":"POST",
		"http_url":"https://example.test/hook",
		"http_headers":"{\"X-Eventic\":\"yes\"}",
		"http_body":"{\"ok\":true}",
		"result_statuses":"200-202",
		"response_mode":"send_data",
		"response_path":"body.id",
		"response_capture":"HOOK_ID",
		"post_action_type":"send_project_message",
		"post_project_repo":"nitecon/target",
		"post_project_message":"Deploy ${HOOK_ID}"
	}`
	rec := httptest.NewRecorder()
	collection.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/workflow-config?scope=global", strings.NewReader(createBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create typed config: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created workflowConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created config: %v", err)
	}
	if created.StepsCount != 1 || created.TypedSteps[0].Type != WorkflowActionHTTPRequest || created.TypedSteps[0].PostActionType != WorkflowActionProjectMessage {
		t.Fatalf("unexpected created typed config: %#v", created)
	}

	full, err := store.GetWorkflow(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if full.Nodes[0].Type != WorkflowActionHTTPRequest || full.Nodes[0].Capture != "HOOK_ID" {
		t.Fatalf("unexpected persisted http node: %#v", full.Nodes[0])
	}
	if !strings.Contains(full.Nodes[0].Config, `"post_actions"`) {
		t.Fatalf("expected post action config, got %s", full.Nodes[0].Config)
	}

	appendBody := `{
		"name":"Notify project",
		"action_type":"send_project_message",
		"project_repo":"nitecon/another",
		"project_message":"Deploy ${HOOK_ID}"
	}`
	stepPath := fmt.Sprintf("/api/workflow-config/%d/steps", created.ID)
	rec = httptest.NewRecorder()
	workflowConfigItemHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, stepPath, strings.NewReader(appendBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("append step: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var appended workflowConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &appended); err != nil {
		t.Fatalf("decode appended config: %v", err)
	}
	if appended.StepsCount != 2 || appended.TypedSteps[1].Type != WorkflowActionProjectMessage {
		t.Fatalf("unexpected appended config: %#v", appended)
	}

	updateBody := `{
		"name":"Trigger project",
		"action_type":"trigger_project_event",
		"project_repo":"nitecon/another",
		"project_event_type":"workflow_dispatch",
		"project_message":"Deploy ${HOOK_ID}"
	}`
	rec = httptest.NewRecorder()
	workflowConfigItemHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, stepPath+"/step-2", strings.NewReader(updateBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update step: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var updated workflowConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated config: %v", err)
	}
	if updated.TypedSteps[1].Type != WorkflowActionTriggerProjectEvent {
		t.Fatalf("expected trigger event step, got %#v", updated.TypedSteps[1])
	}

	rec = httptest.NewRecorder()
	workflowConfigItemHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, stepPath+"/step-2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete step: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var deleted workflowConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode deleted config: %v", err)
	}
	if deleted.StepsCount != 1 {
		t.Fatalf("expected one remaining step, got %#v", deleted)
	}
}

func TestWorkflowCreateRejectsMissingName(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	body := `{"scope":"repo","repo":"nitecon/eventic","event_type":"push","name":""}`
	rec := httptest.NewRecorder()
	workflowsCollectionHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/workflows", strings.NewReader(body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
	if decodeEnvelope(t, rec)["name"] == "" {
		t.Fatal("expected a name field error")
	}
}

func TestWorkflowItemRejectsBadID(t *testing.T) {
	store, err := OpenMemoryProjectStore(t.Context())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	defer store.Close()

	rec := httptest.NewRecorder()
	workflowItemHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/not-a-number", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if decodeEnvelope(t, rec)["id"] == "" {
		t.Fatal("expected an id field error in the envelope")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected json content type, got %q", ct)
	}
}

func TestEventTypesHandlerIncludesComms(t *testing.T) {
	rec := httptest.NewRecorder()
	eventTypesHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/event-types", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var types []struct {
		Event   string   `json:"event"`
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &types); err != nil {
		t.Fatalf("decode event types: %v", err)
	}
	var hasComms, hasPush bool
	for i, et := range types {
		if i > 0 && types[i-1].Event > et.Event {
			t.Fatalf("event types not sorted: %q before %q", types[i-1].Event, et.Event)
		}
		if et.Event == "comms" {
			hasComms = true
		}
		if et.Event == "push" {
			hasPush = true
		}
	}
	if !hasComms {
		t.Fatal("expected comms in event types")
	}
	if !hasPush {
		t.Fatal("expected push in event types")
	}
}

func TestStableEventsAPICreatesCustomEvent(t *testing.T) {
	store := newTestStore(t)
	body := `{"event":"catchall","title":"Catch All","group":"Custom","description":"Route any unmatched event","enabled":true}`
	rec := httptest.NewRecorder()
	stableEventsCollectionHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/stable-events", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var stableEvent StableEventDefinition
	if err := json.Unmarshal(rec.Body.Bytes(), &stableEvent); err != nil {
		t.Fatalf("decode stable event: %v", err)
	}
	if stableEvent.Event != "catchall" || stableEvent.ID == 0 {
		t.Fatalf("unexpected stable event: %#v", stableEvent)
	}

	rec = httptest.NewRecorder()
	stableEventsCollectionHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stable-events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var stableEvents []StableEventDefinition
	if err := json.Unmarshal(rec.Body.Bytes(), &stableEvents); err != nil {
		t.Fatalf("decode stable events: %v", err)
	}
	var found bool
	for _, event := range stableEvents {
		if event.Event == "catchall" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected custom stable event in list")
	}
}

func TestProviderEventsAPIFiltersProvider(t *testing.T) {
	rec := httptest.NewRecorder()
	providerEventsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/provider-events?provider=prometheus", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var events []ProviderEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode provider events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected prometheus provider events")
	}
	for _, event := range events {
		if event.Provider != "prometheus" {
			t.Fatalf("expected prometheus event only, got %#v", event)
		}
	}
}

// decodeEnvelope extracts the ndesign error map from a recorded response.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body struct {
		Errors map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return body.Errors
}

func TestProjectsHandlerReturnsProjectNameList(t *testing.T) {
	ctx := t.Context()
	store, err := OpenProjectStore(ctx, StateConfig{
		Enabled: true,
		Path:    t.TempDir() + "/eventic.db",
	})
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	defer store.Close()

	store.UpsertManagedProject(ctx, "nitecon/eventic", "main", "abc123")
	store.UpsertManagedProject(ctx, "nitecon/another", "main", "def456")

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	rec := httptest.NewRecorder()

	apiProjectsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var projects []string
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("expected json string array response: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	seen := map[string]bool{}
	for _, repo := range projects {
		seen[repo] = true
	}
	for _, repo := range []string{"nitecon/eventic", "nitecon/another"} {
		if !seen[repo] {
			t.Fatalf("expected project %q in %#v", repo, projects)
		}
	}
}

func TestProjectsHandlerReturnsProjectSummariesFilteredAndSorted(t *testing.T) {
	ctx := t.Context()
	store, err := OpenProjectStore(ctx, StateConfig{
		Enabled: true,
		Path:    t.TempDir() + "/eventic.db",
	})
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	defer store.Close()

	store.UpsertManagedProject(ctx, "nitecon/alpha", "main", "aaa111")
	store.UpsertManagedProject(ctx, "nitecon/zulu", "main", "zzz999")
	store.UpsertManagedProject(ctx, "brucehq/bruce", "main", "bbb222")
	newer := testTime().Add(time.Hour)
	older := testTime()
	if _, err := store.db.ExecContext(ctx, `UPDATE projects SET updated_at = ? WHERE repo IN (?, ?)`, newer, "nitecon/alpha", "nitecon/zulu"); err != nil {
		t.Fatalf("set newer timestamps: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE projects SET updated_at = ? WHERE repo = ?`, older, "brucehq/bruce"); err != nil {
		t.Fatalf("set older timestamp: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects?org=nitecon&summary=1", nil)
	rec := httptest.NewRecorder()

	apiProjectsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	var projects []ProjectSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("expected json summary response: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected two nitecon projects, got %#v", projects)
	}
	if projects[0].Repo != "nitecon/zulu" || projects[1].Repo != "nitecon/alpha" {
		t.Fatalf("expected name-desc tie sort within updated_at, got %#v", projects)
	}
	if projects[0].Owner != "nitecon" || projects[0].Name != "zulu" {
		t.Fatalf("expected owner/name split, got %#v", projects[0])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/projects?org=nitecon&search=alp", nil)
	rec = httptest.NewRecorder()
	apiProjectsHandler(store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: expected status 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("search: expected json summary response: %v", err)
	}
	if len(projects) != 1 || projects[0].Repo != "nitecon/alpha" {
		t.Fatalf("expected search to return alpha only, got %#v", projects)
	}
}

func TestRunHookWithOutputExposesReposDir(t *testing.T) {
	ctx := t.Context()
	reposDir := t.TempDir()
	repoPath := filepath.Join(reposDir, "nitecon", "eventic")
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	out, err := RunHookWithOutput(ctx, repoPath, `printf "%s" "$EVENTIC_REPOS/$EVENTIC_REPO"`, "global:post", protocol.EventMsg{
		DeliveryID:  "delivery-1",
		GitHubEvent: "push",
		Repo:        "nitecon/eventic",
	})
	if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	expected := filepath.Join(reposDir, "nitecon", "eventic")
	if out != expected {
		t.Fatalf("expected EVENTIC_REPOS/EVENTIC_REPO %q, got %q", expected, out)
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

func TestSeedFromReposDirAddsDiskRepos(t *testing.T) {
	ctx := t.Context()
	reposDir := t.TempDir()
	repoPath := filepath.Join(reposDir, "nitecon", "eventic")
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	writeFile(t, filepath.Join(repoPath, "README.md"), "# test\n")
	runGit(t, repoPath, "init")
	runGit(t, repoPath, "config", "user.email", "eventic@example.com")
	runGit(t, repoPath, "config", "user.name", "Eventic Test")
	runGit(t, repoPath, "add", ".")
	runGit(t, repoPath, "commit", "-m", "init")

	store, err := OpenProjectStore(ctx, StateConfig{
		Enabled: true,
		Path:    t.TempDir() + "/eventic.db",
	})
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	defer store.Close()

	store.SeedFromReposDir(ctx, reposDir, Config{})

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list seeded projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 seeded project, got %d", len(projects))
	}
	if projects[0] != "nitecon/eventic" {
		t.Fatalf("unexpected seeded project repo: %q", projects[0])
	}

	project, err := store.GetProject(ctx, "nitecon/eventic")
	if err != nil {
		t.Fatalf("get seeded project: %v", err)
	}
	if project.Hash == "" {
		t.Fatal("expected seeded project hash")
	}
}

func testTime() time.Time {
	return time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
