package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

// ── ndesign error envelope ───────────────────────────────────────────────────

// writeAPIError writes the ndesign error envelope `{"errors":{...}}` with the
// supplied status and a JSON content type. The fieldErrs map carries a global
// message under the "error" key and any per-field messages under their field
// names, matching what ndesign markup binds to.
func writeAPIError(w http.ResponseWriter, status int, fieldErrs map[string]string) {
	if fieldErrs == nil {
		fieldErrs = map[string]string{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": fieldErrs})
}

// globalError is shorthand for a single, non-field error message.
func globalError(w http.ResponseWriter, status int, msg string) {
	writeAPIError(w, status, map[string]string{"error": msg})
}

// writeJSON marshals payload as a JSON response with the supplied status.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// ── Workflows ────────────────────────────────────────────────────────────────

// workflowsCollectionHandler serves the workflow collection: GET lists
// workflows (filtered by scope/repo/event) and POST creates a workflow.
func workflowsCollectionHandler(store *ProjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			scope := strings.TrimSpace(r.URL.Query().Get("scope"))
			repo := strings.TrimSpace(r.URL.Query().Get("repo"))
			event := strings.TrimSpace(r.URL.Query().Get("event"))
			workflows, err := store.ListWorkflows(r.Context(), scope, repo, event)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to list workflows")
				return
			}
			if workflows == nil {
				workflows = []Workflow{}
			}
			writeJSON(w, http.StatusOK, workflows)
		case http.MethodPost:
			wf, ok := decodeWorkflow(w, r)
			if !ok {
				return
			}
			if fieldErrs := validateWorkflow(&wf); fieldErrs != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, fieldErrs)
				return
			}
			id, err := store.CreateWorkflow(r.Context(), &wf)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to create workflow")
				return
			}
			created, err := store.GetWorkflow(r.Context(), id)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read created workflow")
				return
			}
			writeJSON(w, http.StatusCreated, created)
		default:
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// workflowItemHandler serves a single workflow by id: GET reads it, PUT updates
// (re-validating the DAG), DELETE removes it.
func workflowItemHandler(store *ProjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "/api/workflows/")
		if !ok {
			return
		}
		switch r.Method {
		case http.MethodGet:
			wf, err := store.GetWorkflow(r.Context(), id)
			if isNotFound(err) {
				globalError(w, http.StatusNotFound, "workflow not found")
				return
			}
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read workflow")
				return
			}
			writeJSON(w, http.StatusOK, wf)
		case http.MethodPut:
			wf, decoded := decodeWorkflow(w, r)
			if !decoded {
				return
			}
			wf.ID = id
			if fieldErrs := validateWorkflow(&wf); fieldErrs != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, fieldErrs)
				return
			}
			err := store.UpdateWorkflow(r.Context(), &wf)
			if isNotFound(err) {
				globalError(w, http.StatusNotFound, "workflow not found")
				return
			}
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to update workflow")
				return
			}
			updated, err := store.GetWorkflow(r.Context(), id)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read updated workflow")
				return
			}
			writeJSON(w, http.StatusOK, updated)
		case http.MethodDelete:
			if err := store.DeleteWorkflow(r.Context(), id); err != nil {
				globalError(w, http.StatusInternalServerError, "failed to delete workflow")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		default:
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// workflowConfig is a dashboard-friendly projection of Workflow that flattens
// command nodes into an editable newline-delimited step list.
type workflowConfig struct {
	ID         int64                `json:"id"`
	Scope      string               `json:"scope"`
	Repo       string               `json:"repo"`
	EventType  string               `json:"event_type"`
	Name       string               `json:"name"`
	Enabled    bool                 `json:"enabled"`
	Steps      string               `json:"steps"`
	TypedSteps []workflowStepConfig `json:"typed_steps"`
	StepsCount int                  `json:"steps_count"`
	UpdatedAt  time.Time            `json:"updated_at"`
}

type workflowConfigRequest struct {
	Scope               string                `json:"scope"`
	Repo                string                `json:"repo"`
	EventType           string                `json:"event_type"`
	Name                string                `json:"name"`
	Steps               string                `json:"steps"`
	TypedSteps          []workflowStepRequest `json:"typed_steps"`
	StepName            string                `json:"step_name"`
	ActionType          string                `json:"action_type"`
	Type                string                `json:"type"`
	Command             string                `json:"command"`
	HTTPMethod          string                `json:"http_method"`
	HTTPURL             string                `json:"http_url"`
	HTTPHeaders         string                `json:"http_headers"`
	HTTPBody            string                `json:"http_body"`
	ProjectRepo         string                `json:"project_repo"`
	ProjectRef          string                `json:"project_ref"`
	ProjectMessage      string                `json:"project_message"`
	ProjectEvent        string                `json:"project_event_type"`
	ResultStatuses      string                `json:"result_statuses"`
	ResultExitCodes     string                `json:"result_exit_codes"`
	ResponseMode        string                `json:"response_mode"`
	ResponsePath        string                `json:"response_path"`
	ResponseCapture     string                `json:"response_capture"`
	PostActionName      string                `json:"post_action_name"`
	PostActionType      string                `json:"post_action_type"`
	PostCommand         string                `json:"post_command"`
	PostHTTPMethod      string                `json:"post_http_method"`
	PostHTTPURL         string                `json:"post_http_url"`
	PostHTTPHeaders     string                `json:"post_http_headers"`
	PostHTTPBody        string                `json:"post_http_body"`
	PostProjectRepo     string                `json:"post_project_repo"`
	PostProjectRef      string                `json:"post_project_ref"`
	PostProjectMsg      string                `json:"post_project_message"`
	PostProjectEvent    string                `json:"post_project_event_type"`
	PostResultStatuses  string                `json:"post_result_statuses"`
	PostResultExitCodes string                `json:"post_result_exit_codes"`
	PostContinueOnError boolish               `json:"post_continue_on_error"`
	PostTimeoutSeconds  intish                `json:"post_timeout_seconds"`
	ContinueOnError     boolish               `json:"continue_on_error"`
	TimeoutSeconds      intish                `json:"timeout_seconds"`
}

func workflowConfigCollectionHandler(store *ProjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			globalError(w, http.StatusInternalServerError, "project store disabled")
			return
		}

		switch r.Method {
		case http.MethodGet:
			scope := strings.TrimSpace(r.URL.Query().Get("scope"))
			repo := strings.TrimSpace(r.URL.Query().Get("repo"))
			configs, err := listWorkflowConfigs(r.Context(), store, scope, repo)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to list workflow configuration")
				return
			}
			writeJSON(w, http.StatusOK, configs)
		case http.MethodPost:
			req, ok := decodeWorkflowConfig(w, r)
			if !ok {
				return
			}
			scope := firstNonEmpty(r.URL.Query().Get("scope"), req.Scope)
			repo := firstNonEmpty(r.URL.Query().Get("repo"), req.Repo)
			wf, fieldErrs := workflowFromConfig(req, scope, repo, nil)
			if fieldErrs != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, fieldErrs)
				return
			}
			id, err := store.CreateWorkflow(r.Context(), &wf)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to create workflow")
				return
			}
			created, err := store.GetWorkflow(r.Context(), id)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read created workflow")
				return
			}
			writeJSON(w, http.StatusCreated, workflowConfigFrom(*created))
		default:
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func workflowConfigItemHandler(store *ProjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			globalError(w, http.StatusInternalServerError, "project store disabled")
			return
		}

		id, stepPath, ok := parseWorkflowConfigPath(w, r)
		if !ok {
			return
		}
		if stepPath != "" {
			workflowConfigStepHandler(store, id, stepPath).ServeHTTP(w, r)
			return
		}

		switch r.Method {
		case http.MethodPut:
			existing, err := store.GetWorkflow(r.Context(), id)
			if isNotFound(err) {
				globalError(w, http.StatusNotFound, "workflow not found")
				return
			}
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read workflow")
				return
			}
			req, ok := decodeWorkflowConfig(w, r)
			if !ok {
				return
			}
			wf, fieldErrs := workflowFromConfig(req, existing.Scope, existing.Repo, existing)
			if fieldErrs != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, fieldErrs)
				return
			}
			wf.ID = id
			if err := store.UpdateWorkflow(r.Context(), &wf); err != nil {
				globalError(w, http.StatusInternalServerError, "failed to update workflow")
				return
			}
			updated, err := store.GetWorkflow(r.Context(), id)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read updated workflow")
				return
			}
			writeJSON(w, http.StatusOK, workflowConfigFrom(*updated))
		case http.MethodDelete:
			if err := store.DeleteWorkflow(r.Context(), id); err != nil {
				globalError(w, http.StatusInternalServerError, "failed to delete workflow")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		default:
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func listWorkflowConfigs(ctx context.Context, store *ProjectStore, scope, repo string) ([]workflowConfig, error) {
	workflows, err := store.ListWorkflows(ctx, strings.TrimSpace(scope), strings.TrimSpace(repo), "")
	if err != nil {
		return nil, err
	}
	configs := make([]workflowConfig, 0, len(workflows))
	for _, wf := range workflows {
		full, err := store.GetWorkflow(ctx, wf.ID)
		if err != nil || full == nil {
			configs = append(configs, workflowConfigFrom(wf))
			continue
		}
		configs = append(configs, workflowConfigFrom(*full))
	}
	return configs, nil
}

func decodeWorkflowConfig(w http.ResponseWriter, r *http.Request) (workflowConfigRequest, bool) {
	var req workflowConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		globalError(w, http.StatusBadRequest, "invalid workflow configuration payload")
		return workflowConfigRequest{}, false
	}
	return req, true
}

func workflowFromConfig(req workflowConfigRequest, scope, repo string, existing *Workflow) (Workflow, map[string]string) {
	name := strings.TrimSpace(req.Name)
	eventType := strings.TrimSpace(req.EventType)
	if existing != nil {
		if name == "" {
			name = existing.Name
		}
		if eventType == "" {
			eventType = existing.EventType
		}
	}

	enabled := true
	if existing != nil {
		enabled = existing.Enabled
	}

	nodes, edges, stepsErr := workflowGraphFromConfigRequest(req, existing)
	wf := Workflow{
		Scope:     strings.TrimSpace(scope),
		Repo:      strings.TrimSpace(repo),
		EventType: eventType,
		Name:      name,
		Enabled:   enabled,
		Nodes:     nodes,
		Edges:     edges,
	}
	fieldErrs := validateWorkflow(&wf)
	if fieldErrs == nil {
		fieldErrs = map[string]string{}
	}
	if stepsErr != "" {
		fieldErrs["steps"] = stepsErr
	}
	if len(fieldErrs) > 0 {
		return Workflow{}, fieldErrs
	}
	return wf, nil
}

func workflowGraphFromConfigRequest(req workflowConfigRequest, existing *Workflow) ([]WorkflowNode, []WorkflowEdge, string) {
	switch {
	case len(req.TypedSteps) > 0:
		nodes := make([]WorkflowNode, 0, len(req.TypedSteps))
		for i, stepReq := range req.TypedSteps {
			node, msg := workflowNodeFromStepRequest(stepReq, i+1, nil)
			if msg != "" {
				return nil, nil, msg
			}
			if hasNodeKey(nodes, node.NodeKey) {
				return nil, nil, fmt.Sprintf("duplicate node_key %q", node.NodeKey)
			}
			nodes = append(nodes, node)
		}
		return nodes, linearWorkflowEdges(nodes), ""
	case hasWorkflowStepPayload(workflowStepRequestFromConfig(req)):
		node, msg := workflowNodeFromStepRequest(workflowStepRequestFromConfig(req), 1, nil)
		if msg != "" {
			return nil, nil, msg
		}
		return []WorkflowNode{node}, nil, ""
	case strings.TrimSpace(req.Steps) != "":
		return parseWorkflowConfigSteps(req.Steps)
	case existing != nil:
		return existing.Nodes, existing.Edges, ""
	default:
		return nil, nil, "at least one step is required"
	}
}

func parseWorkflowConfigSteps(steps string) ([]WorkflowNode, []WorkflowEdge, string) {
	lines := strings.Split(steps, "\n")
	nodes := make([]WorkflowNode, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := ""
		command := line
		if before, after, ok := strings.Cut(line, "|"); ok {
			name = strings.TrimSpace(before)
			command = strings.TrimSpace(after)
		}
		if command == "" {
			return nil, nil, "each step requires a command"
		}
		index := len(nodes) + 1
		nodes = append(nodes, WorkflowNode{
			NodeKey: fmt.Sprintf("step-%d", index),
			Name:    name,
			Type:    WorkflowActionRunCommand,
			Command: command,
			PosX:    float64((index - 1) * 220),
			PosY:    0,
		})
	}
	if len(nodes) == 0 {
		return nil, nil, "at least one step is required"
	}

	edges := make([]WorkflowEdge, 0, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		edges = append(edges, WorkflowEdge{
			FromNode:  nodes[i-1].NodeKey,
			ToNode:    nodes[i].NodeKey,
			Condition: "success",
		})
	}
	return nodes, edges, ""
}

func workflowConfigStepHandler(store *ProjectStore, id int64, stepPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(stepPath, "/"), "/")
		if len(parts) == 0 || parts[0] != "steps" {
			globalError(w, http.StatusNotFound, "not found")
			return
		}

		wf, err := store.GetWorkflow(r.Context(), id)
		if isNotFound(err) {
			globalError(w, http.StatusNotFound, "workflow not found")
			return
		}
		if err != nil {
			globalError(w, http.StatusInternalServerError, "failed to read workflow")
			return
		}

		switch {
		case len(parts) == 1 && r.Method == http.MethodPost:
			req, ok := decodeWorkflowStepRequest(w, r)
			if !ok {
				return
			}
			if req.NodeKey == "" {
				req.NodeKey = nextWorkflowStepKey(wf.Nodes)
			}
			node, msg := workflowNodeFromStepRequest(req, len(wf.Nodes)+1, nil)
			if msg != "" {
				writeAPIError(w, http.StatusUnprocessableEntity, map[string]string{"steps": msg})
				return
			}
			if hasNodeKey(wf.Nodes, node.NodeKey) {
				writeAPIError(w, http.StatusUnprocessableEntity, map[string]string{"node_key": "already exists"})
				return
			}
			wf.Nodes = append(wf.Nodes, node)
			wf.Edges = linearWorkflowEdges(wf.Nodes)
			if fieldErrs := validateWorkflow(wf); fieldErrs != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, fieldErrs)
				return
			}
			if err := store.UpdateWorkflow(r.Context(), wf); err != nil {
				globalError(w, http.StatusInternalServerError, "failed to append workflow step")
				return
			}
			updated, err := store.GetWorkflow(r.Context(), id)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read updated workflow")
				return
			}
			writeJSON(w, http.StatusCreated, workflowConfigFrom(*updated))
		case len(parts) == 2 && r.Method == http.MethodPut:
			nodeKey := parts[1]
			index := workflowNodeIndex(wf.Nodes, nodeKey)
			if index < 0 {
				globalError(w, http.StatusNotFound, "workflow step not found")
				return
			}
			req, ok := decodeWorkflowStepRequest(w, r)
			if !ok {
				return
			}
			req.NodeKey = nodeKey
			node, msg := workflowNodeFromStepRequest(req, index+1, &wf.Nodes[index])
			if msg != "" {
				writeAPIError(w, http.StatusUnprocessableEntity, map[string]string{"steps": msg})
				return
			}
			wf.Nodes[index] = node
			wf.Edges = linearWorkflowEdges(wf.Nodes)
			if fieldErrs := validateWorkflow(wf); fieldErrs != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, fieldErrs)
				return
			}
			if err := store.UpdateWorkflow(r.Context(), wf); err != nil {
				globalError(w, http.StatusInternalServerError, "failed to update workflow step")
				return
			}
			updated, err := store.GetWorkflow(r.Context(), id)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read updated workflow")
				return
			}
			writeJSON(w, http.StatusOK, workflowConfigFrom(*updated))
		case len(parts) == 2 && r.Method == http.MethodDelete:
			nodeKey := parts[1]
			index := workflowNodeIndex(wf.Nodes, nodeKey)
			if index < 0 {
				globalError(w, http.StatusNotFound, "workflow step not found")
				return
			}
			wf.Nodes = append(wf.Nodes[:index], wf.Nodes[index+1:]...)
			if len(wf.Nodes) == 0 {
				writeAPIError(w, http.StatusUnprocessableEntity, map[string]string{"steps": "at least one step is required"})
				return
			}
			wf.Edges = linearWorkflowEdges(wf.Nodes)
			if fieldErrs := validateWorkflow(wf); fieldErrs != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, fieldErrs)
				return
			}
			if err := store.UpdateWorkflow(r.Context(), wf); err != nil {
				globalError(w, http.StatusInternalServerError, "failed to delete workflow step")
				return
			}
			updated, err := store.GetWorkflow(r.Context(), id)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read updated workflow")
				return
			}
			writeJSON(w, http.StatusOK, workflowConfigFrom(*updated))
		default:
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func decodeWorkflowStepRequest(w http.ResponseWriter, r *http.Request) (workflowStepRequest, bool) {
	var req workflowStepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		globalError(w, http.StatusBadRequest, "invalid workflow step payload")
		return workflowStepRequest{}, false
	}
	return req, true
}

func hasWorkflowStepPayload(req workflowStepRequest) bool {
	return strings.TrimSpace(firstNonEmpty(
		req.ActionType,
		req.Type,
		req.Command,
		req.HTTPURL,
		req.ProjectRepo,
		req.ProjectMessage,
		req.ProjectEvent,
	)) != ""
}

func workflowStepRequestFromConfig(req workflowConfigRequest) workflowStepRequest {
	return workflowStepRequest{
		StepName:            req.StepName,
		Type:                req.Type,
		ActionType:          req.ActionType,
		Command:             req.Command,
		HTTPMethod:          req.HTTPMethod,
		HTTPURL:             req.HTTPURL,
		HTTPHeaders:         req.HTTPHeaders,
		HTTPBody:            req.HTTPBody,
		ProjectRepo:         req.ProjectRepo,
		ProjectRef:          req.ProjectRef,
		ProjectMessage:      req.ProjectMessage,
		ProjectEvent:        req.ProjectEvent,
		ResultStatuses:      req.ResultStatuses,
		ResultExitCodes:     req.ResultExitCodes,
		ResponseMode:        req.ResponseMode,
		ResponsePath:        req.ResponsePath,
		ResponseCapture:     req.ResponseCapture,
		PostActionName:      req.PostActionName,
		PostActionType:      req.PostActionType,
		PostCommand:         req.PostCommand,
		PostHTTPMethod:      req.PostHTTPMethod,
		PostHTTPURL:         req.PostHTTPURL,
		PostHTTPHeaders:     req.PostHTTPHeaders,
		PostHTTPBody:        req.PostHTTPBody,
		PostProjectRepo:     req.PostProjectRepo,
		PostProjectRef:      req.PostProjectRef,
		PostProjectMsg:      req.PostProjectMsg,
		PostProjectEvent:    req.PostProjectEvent,
		PostResultStatuses:  req.PostResultStatuses,
		PostResultExitCodes: req.PostResultExitCodes,
		PostContinueOnError: req.PostContinueOnError,
		PostTimeoutSeconds:  req.PostTimeoutSeconds,
		ContinueOnError:     req.ContinueOnError,
		TimeoutSeconds:      req.TimeoutSeconds,
	}
}

func linearWorkflowEdges(nodes []WorkflowNode) []WorkflowEdge {
	if len(nodes) < 2 {
		return nil
	}
	edges := make([]WorkflowEdge, 0, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		edges = append(edges, WorkflowEdge{
			FromNode:  nodes[i-1].NodeKey,
			ToNode:    nodes[i].NodeKey,
			Condition: NodeStateSuccess,
		})
	}
	return edges
}

func hasNodeKey(nodes []WorkflowNode, key string) bool {
	return workflowNodeIndex(nodes, key) >= 0
}

func workflowNodeIndex(nodes []WorkflowNode, key string) int {
	for i, node := range nodes {
		if node.NodeKey == key {
			return i
		}
	}
	return -1
}

func nextWorkflowStepKey(nodes []WorkflowNode) string {
	for i := len(nodes) + 1; ; i++ {
		key := fmt.Sprintf("step-%d", i)
		if !hasNodeKey(nodes, key) {
			return key
		}
	}
}

func workflowConfigFrom(wf Workflow) workflowConfig {
	steps := make([]string, 0, len(wf.Nodes))
	typedSteps := make([]workflowStepConfig, 0, len(wf.Nodes))
	for _, node := range wf.Nodes {
		step := workflowStepConfigFromNode(node)
		typedSteps = append(typedSteps, step)
		command := strings.TrimSpace(step.Summary)
		if name := strings.TrimSpace(node.Name); name != "" {
			steps = append(steps, name+" | "+command)
			continue
		}
		steps = append(steps, command)
	}
	return workflowConfig{
		ID:         wf.ID,
		Scope:      wf.Scope,
		Repo:       wf.Repo,
		EventType:  wf.EventType,
		Name:       wf.Name,
		Enabled:    wf.Enabled,
		Steps:      strings.Join(steps, "\n"),
		TypedSteps: typedSteps,
		StepsCount: len(typedSteps),
		UpdatedAt:  wf.UpdatedAt,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// decodeWorkflow decodes a Workflow from the request body, writing a 400
// envelope and reporting false on malformed JSON.
func decodeWorkflow(w http.ResponseWriter, r *http.Request) (Workflow, bool) {
	var wf Workflow
	if err := json.NewDecoder(r.Body).Decode(&wf); err != nil {
		globalError(w, http.StatusBadRequest, "invalid workflow payload")
		return Workflow{}, false
	}
	return wf, true
}

// validateWorkflow checks a workflow's metadata, nodes, and DAG integrity. It
// returns nil when valid, or a field-keyed error map for the ndesign envelope.
// The "error" key carries graph-level (non-field) failures such as cycles.
func validateWorkflow(wf *Workflow) map[string]string {
	fieldErrs := map[string]string{}

	wf.Name = strings.TrimSpace(wf.Name)
	wf.Scope = strings.TrimSpace(wf.Scope)
	wf.Repo = strings.TrimSpace(wf.Repo)
	wf.EventType = strings.TrimSpace(wf.EventType)

	if wf.Name == "" {
		fieldErrs["name"] = "required"
	}
	switch wf.Scope {
	case WorkflowScopeRepo:
		if wf.Repo == "" {
			fieldErrs["repo"] = "required for repo-scoped workflows"
		} else if strings.Count(wf.Repo, "/") != 1 {
			fieldErrs["repo"] = "must be in org/repo form"
		}
	case WorkflowScopeGlobal:
		// Global workflows must not be pinned to a repo.
		wf.Repo = ""
	default:
		fieldErrs["scope"] = `must be "repo" or "global"`
	}
	if wf.EventType == "" {
		fieldErrs["event_type"] = "required"
	}

	if nodeErr := validateWorkflowNodes(wf.Nodes); nodeErr != "" {
		fieldErrs["nodes"] = nodeErr
	}

	// Graph-level validation (cycles, dangling edges) only runs once the node
	// set itself is well-formed, otherwise BuildGraph reports redundant errors.
	if _, ok := fieldErrs["nodes"]; !ok {
		graph, err := BuildGraph(wf.Nodes, wf.Edges)
		if err != nil {
			fieldErrs["error"] = err.Error()
		} else if err := Validate(graph); err != nil {
			fieldErrs["error"] = err.Error()
		}
	}

	if len(fieldErrs) == 0 {
		return nil
	}
	return fieldErrs
}

// validateWorkflowNodes checks node-key uniqueness/non-emptiness and validates
// each typed action's required fields. It returns "" when the nodes are valid.
func validateWorkflowNodes(nodes []WorkflowNode) string {
	seen := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		key := strings.TrimSpace(nodes[i].NodeKey)
		if key == "" {
			return "every node requires a node_key"
		}
		if _, dup := seen[key]; dup {
			return fmt.Sprintf("duplicate node_key %q", key)
		}
		seen[key] = struct{}{}

		nodes[i].Type = normalizeActionType(nodes[i].Type)
		if err := validateWorkflowNode(nodes[i]); err != nil {
			return err.Error()
		}
	}
	return ""
}

// ── Event types ──────────────────────────────────────────────────────────────

// eventType is one entry in the GET /api/event-types reference list.
type eventType struct {
	Event   string   `json:"event"`
	Actions []string `json:"actions"`
}

// eventTypesHandler returns the GitHub webhook event reference plus the synthetic
// "comms" event, sorted by event name, for the workflow editor's dropdowns.
func eventTypesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		types := make([]eventType, 0, len(GitHubWebhookEvents)+1)
		for event, actions := range GitHubWebhookEvents {
			sorted := append([]string(nil), actions...)
			sort.Strings(sorted)
			types = append(types, eventType{Event: event, Actions: sorted})
		}
		// The comms event is injected by external agents and carries no action.
		types = append(types, eventType{Event: "comms", Actions: []string{}})
		sort.Slice(types, func(i, j int) bool { return types[i].Event < types[j].Event })
		writeJSON(w, http.StatusOK, types)
	}
}

// ── Projects ─────────────────────────────────────────────────────────────────

// apiProjectsHandler serves project summaries under /api/projects, preserving
// the legacy shapes: a string array for the plain list and a ProjectState for a
// repo. Supplying summary=1, org, or search returns ProjectSummary objects for
// navigation and overview filtering.
func apiProjectsHandler(store *ProjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		repo := strings.TrimPrefix(r.URL.Path, "/api/projects/")
		if repo != r.URL.Path {
			repo = strings.Trim(repo, "/")
			if repo == "" || strings.Count(repo, "/") != 1 {
				globalError(w, http.StatusNotFound, "project not found")
				return
			}
			project, err := store.GetProject(r.Context(), repo)
			if isNotFound(err) {
				globalError(w, http.StatusNotFound, "project not found")
				return
			}
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read project")
				return
			}
			writeJSON(w, http.StatusOK, project)
			return
		}

		query := r.URL.Query()
		org := strings.TrimSpace(query.Get("org"))
		search := strings.TrimSpace(query.Get("search"))
		if query.Get("summary") == "1" || org != "" || search != "" {
			projects, err := store.ListProjectSummaries(r.Context(), org, search)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to read projects")
				return
			}
			if projects == nil {
				projects = []ProjectSummary{}
			}
			writeJSON(w, http.StatusOK, projects)
			return
		}

		projects, err := store.ListProjects(r.Context())
		if err != nil {
			globalError(w, http.StatusInternalServerError, "failed to read projects")
			return
		}
		if projects == nil {
			projects = []string{}
		}
		writeJSON(w, http.StatusOK, projects)
	}
}

// ── Refs ─────────────────────────────────────────────────────────────────────

// apiRefsHandler lists git branches and tags for a checked-out repo.
func apiRefsHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		repo := strings.TrimSpace(r.URL.Query().Get("repo"))
		repoPath, err := repoPathForWeb(cfg.ReposDir, repo)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, map[string]string{"repo": "invalid or not checked out"})
			return
		}
		refs, err := listReplayRefs(r.Context(), repoPath)
		if err != nil {
			globalError(w, http.StatusInternalServerError, "failed to list refs")
			return
		}
		writeJSON(w, http.StatusOK, refs)
	}
}

// ── Runs ─────────────────────────────────────────────────────────────────────

// triggerRequest is the manual-trigger body for POST /api/runs.
type triggerRequest struct {
	Repo      string `json:"repo"`
	EventType string `json:"event_type"`
	Ref       string `json:"ref"`
	Message   string `json:"message"`
}

// runsCollectionHandler serves the run collection: GET lists recent runs and
// POST manually triggers a workflow via the replay dispatcher.
func runsCollectionHandler(store *ProjectStore, replay ReplayDispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			repo := strings.TrimSpace(r.URL.Query().Get("repo"))
			limit := 0
			if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
				if parsed, err := strconv.Atoi(raw); err == nil {
					limit = parsed
				}
			}
			runs, err := store.ListRuns(r.Context(), repo, limit)
			if err != nil {
				globalError(w, http.StatusInternalServerError, "failed to list runs")
				return
			}
			if runs == nil {
				runs = []WorkflowRun{}
			}
			writeJSON(w, http.StatusOK, runs)
		case http.MethodPost:
			triggerRun(w, r, replay)
		default:
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// triggerRun validates a manual-trigger request and dispatches a synthetic
// EventMsg through the replay path, returning 202 with the delivery id.
func triggerRun(w http.ResponseWriter, r *http.Request, replay ReplayDispatcher) {
	if replay == nil {
		globalError(w, http.StatusServiceUnavailable, "manual trigger unavailable")
		return
	}

	var req triggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		globalError(w, http.StatusBadRequest, "invalid trigger payload")
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.EventType = strings.TrimSpace(req.EventType)
	req.Ref = strings.TrimSpace(req.Ref)

	fieldErrs := map[string]string{}
	if req.Repo == "" {
		fieldErrs["repo"] = "required"
	} else if strings.Count(req.Repo, "/") != 1 {
		fieldErrs["repo"] = "must be in org/repo form"
	}
	if req.EventType == "" {
		fieldErrs["event_type"] = "required"
	}
	if req.Ref == "" {
		fieldErrs["ref"] = "required"
	}
	if len(fieldErrs) > 0 {
		writeAPIError(w, http.StatusBadRequest, fieldErrs)
		return
	}

	eventName, action := splitEventType(req.EventType)
	deliveryID := fmt.Sprintf("manual-%d", time.Now().UnixNano())
	event := protocol.EventMsg{
		MsgType:     "Event",
		DeliveryID:  deliveryID,
		GitHubEvent: eventName,
		Repo:        req.Repo,
		Ref:         req.Ref,
		Action:      action,
		Message:     req.Message,
		Sender:      "eventic-web",
		CloneURL:    "https://github.com/" + req.Repo + ".git",
		Payload:     replayPayload(req.Repo, req.Ref, eventName, action, deliveryID),
	}

	replay(r.Context(), event)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"delivery_id": deliveryID,
		"status":      "accepted",
	})
}

// runItemHandler returns a single run with its per-node detail.
func runItemHandler(store *ProjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Guard the streaming sub-path so it is not parsed as a run id.
		if r.URL.Path == "/api/runs/stream" {
			globalError(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodGet {
			globalError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		id, ok := parsePathID(w, r, "/api/runs/")
		if !ok {
			return
		}
		run, err := store.GetRun(r.Context(), id)
		if isNotFound(err) {
			globalError(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			globalError(w, http.StatusInternalServerError, "failed to read run")
			return
		}
		writeJSON(w, http.StatusOK, run)
	}
}

// parsePathID extracts and parses a trailing int64 id from a request path that
// begins with prefix, writing a 400 envelope on a malformed id.
func parsePathID(w http.ResponseWriter, r *http.Request, prefix string) (int64, bool) {
	raw := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, map[string]string{"id": "invalid id"})
		return 0, false
	}
	return id, true
}

func parseWorkflowConfigPath(w http.ResponseWriter, r *http.Request) (id int64, suffix string, ok bool) {
	raw := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/workflow-config/"), "/")
	idPart, rest, _ := strings.Cut(raw, "/")
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, map[string]string{"id": "invalid id"})
		return 0, "", false
	}
	return id, strings.Trim(rest, "/"), true
}

// ── Live WebSocket stream ────────────────────────────────────────────────────

// wsRunMessage is the JSON frame pushed over /ws/runs. Type is always "run" and
// is the value ndesign markup matches via data-nd-ws-filter; the embedded
// ExecutionEvent carries the full per-node hooks[] detail.
//
// We emit a single "run" stream (rather than splitting "run"/"node") because an
// ExecutionEvent already aggregates its node updates under hooks[]; a separate
// "node" frame would duplicate state the client already receives.
type wsRunMessage struct {
	Type string `json:"type"`
	ExecutionEvent
}

// wsRunsHandler upgrades to a WebSocket and streams execution events. Auth: when
// cfg.Token is set the "?token=" query must match (401 before upgrade); when
// unset the stream is open. An optional "?repo=" filters to a single repo. No
// subscribe frame is expected — events stream immediately on connect, beginning
// with the current snapshot.
func wsRunsHandler(cfg WebConfig, logStore *ExecutionLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Token != "" && r.URL.Query().Get("token") != cfg.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		repoFilter := strings.TrimSpace(r.URL.Query().Get("repo"))

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			log.Debug().Err(err).Msg("ws runs accept failed")
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "closing")

		ctx := r.Context()
		ch, cancel := logStore.Subscribe()
		defer cancel()

		// Send the current snapshot before live updates so a reconnecting client
		// immediately repaints. Snapshot is newest-first; reverse to oldest-first.
		snapshot := logStore.Events()
		for i := len(snapshot) - 1; i >= 0; i-- {
			if !wsForward(ctx, conn, snapshot[i], repoFilter) {
				return
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				if !wsForward(ctx, conn, event, repoFilter) {
					return
				}
			}
		}
	}
}

// wsForward writes one execution event as a "run" frame, honoring the optional
// repo filter. It reports false when the connection can no longer be written.
func wsForward(ctx context.Context, conn *websocket.Conn, event ExecutionEvent, repoFilter string) bool {
	if repoFilter != "" && event.Repo != repoFilter {
		return true
	}
	data, err := json.Marshal(wsRunMessage{Type: "run", ExecutionEvent: event})
	if err != nil {
		return true
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return false
	}
	return true
}

// indexHandler and the template shell have been moved to web_templates.go.
