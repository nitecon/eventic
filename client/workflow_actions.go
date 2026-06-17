package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nitecon/eventic/protocol"
)

const (
	WorkflowActionRunCommand          = "run_command"
	WorkflowActionLegacyCommand       = "command"
	WorkflowActionHTTPRequest         = "send_http_request"
	WorkflowActionProjectMessage      = "send_project_message"
	WorkflowActionTriggerProjectEvent = "trigger_project_event"

	ResponseModeNoop        = "noop"
	ResponseModeSendData    = "send_data"
	ResponseModeIterateData = "iterate_data"
)

type workflowActionConfig struct {
	HTTPRequest    *httpRequestActionConfig    `json:"http_request,omitempty"`
	ProjectMessage *projectMessageActionConfig `json:"project_message,omitempty"`
	ProjectEvent   *projectEventActionConfig   `json:"project_event,omitempty"`
	Result         resultEvaluationConfig      `json:"result,omitempty"`
	Response       responseHandlingConfig      `json:"response,omitempty"`
	PostActions    []workflowPostActionConfig  `json:"post_actions,omitempty"`
}

type httpRequestActionConfig struct {
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type projectMessageActionConfig struct {
	Repo     string `json:"repo,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Message  string `json:"message,omitempty"`
	Sender   string `json:"sender,omitempty"`
	CloneURL string `json:"clone_url,omitempty"`
}

type projectEventActionConfig struct {
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
	EventType string `json:"event_type,omitempty"`
	Message   string `json:"message,omitempty"`
	Sender    string `json:"sender,omitempty"`
	CloneURL  string `json:"clone_url,omitempty"`
}

type resultEvaluationConfig struct {
	SuccessStatuses  string `json:"success_statuses,omitempty"`
	SuccessExitCodes string `json:"success_exit_codes,omitempty"`
}

type responseHandlingConfig struct {
	Mode    string `json:"mode,omitempty"`
	Path    string `json:"path,omitempty"`
	Capture string `json:"capture,omitempty"`
}

type workflowPostActionConfig struct {
	Name            string                      `json:"name,omitempty"`
	Type            string                      `json:"type,omitempty"`
	Command         string                      `json:"command,omitempty"`
	HTTPRequest     *httpRequestActionConfig    `json:"http_request,omitempty"`
	ProjectMessage  *projectMessageActionConfig `json:"project_message,omitempty"`
	ProjectEvent    *projectEventActionConfig   `json:"project_event,omitempty"`
	Result          resultEvaluationConfig      `json:"result,omitempty"`
	ContinueOnError bool                        `json:"continue_on_error,omitempty"`
	TimeoutSeconds  int64                       `json:"timeout_seconds,omitempty"`
}

type workflowStepRequest struct {
	NodeKey             string  `json:"node_key"`
	Name                string  `json:"name"`
	StepName            string  `json:"step_name"`
	Type                string  `json:"type"`
	ActionType          string  `json:"action_type"`
	Command             string  `json:"command"`
	HTTPMethod          string  `json:"http_method"`
	HTTPURL             string  `json:"http_url"`
	HTTPHeaders         string  `json:"http_headers"`
	HTTPBody            string  `json:"http_body"`
	ProjectRepo         string  `json:"project_repo"`
	ProjectRef          string  `json:"project_ref"`
	ProjectMessage      string  `json:"project_message"`
	ProjectEvent        string  `json:"project_event_type"`
	ResultStatuses      string  `json:"result_statuses"`
	ResultExitCodes     string  `json:"result_exit_codes"`
	ResponseMode        string  `json:"response_mode"`
	ResponsePath        string  `json:"response_path"`
	ResponseCapture     string  `json:"response_capture"`
	PostActionName      string  `json:"post_action_name"`
	PostActionType      string  `json:"post_action_type"`
	PostCommand         string  `json:"post_command"`
	PostHTTPMethod      string  `json:"post_http_method"`
	PostHTTPURL         string  `json:"post_http_url"`
	PostHTTPHeaders     string  `json:"post_http_headers"`
	PostHTTPBody        string  `json:"post_http_body"`
	PostProjectRepo     string  `json:"post_project_repo"`
	PostProjectRef      string  `json:"post_project_ref"`
	PostProjectMsg      string  `json:"post_project_message"`
	PostProjectEvent    string  `json:"post_project_event_type"`
	PostResultStatuses  string  `json:"post_result_statuses"`
	PostResultExitCodes string  `json:"post_result_exit_codes"`
	PostContinueOnError boolish `json:"post_continue_on_error"`
	PostTimeoutSeconds  intish  `json:"post_timeout_seconds"`
	ContinueOnError     boolish `json:"continue_on_error"`
	TimeoutSeconds      intish  `json:"timeout_seconds"`
}

type workflowStepConfig struct {
	NodeKey             string `json:"node_key"`
	Name                string `json:"name"`
	Type                string `json:"type"`
	TypeLabel           string `json:"type_label"`
	Summary             string `json:"summary"`
	Command             string `json:"command,omitempty"`
	HTTPMethod          string `json:"http_method,omitempty"`
	HTTPURL             string `json:"http_url,omitempty"`
	HTTPHeaders         string `json:"http_headers,omitempty"`
	HTTPBody            string `json:"http_body,omitempty"`
	ProjectRepo         string `json:"project_repo,omitempty"`
	ProjectRef          string `json:"project_ref,omitempty"`
	ProjectMessage      string `json:"project_message,omitempty"`
	ProjectEvent        string `json:"project_event_type,omitempty"`
	ResultStatuses      string `json:"result_statuses,omitempty"`
	ResultExitCodes     string `json:"result_exit_codes,omitempty"`
	ResponseMode        string `json:"response_mode"`
	ResponsePath        string `json:"response_path,omitempty"`
	ResponseCapture     string `json:"response_capture,omitempty"`
	PostActionName      string `json:"post_action_name,omitempty"`
	PostActionType      string `json:"post_action_type,omitempty"`
	PostCommand         string `json:"post_command,omitempty"`
	PostHTTPMethod      string `json:"post_http_method,omitempty"`
	PostHTTPURL         string `json:"post_http_url,omitempty"`
	PostHTTPHeaders     string `json:"post_http_headers,omitempty"`
	PostHTTPBody        string `json:"post_http_body,omitempty"`
	PostProjectRepo     string `json:"post_project_repo,omitempty"`
	PostProjectRef      string `json:"post_project_ref,omitempty"`
	PostProjectMsg      string `json:"post_project_message,omitempty"`
	PostProjectEvent    string `json:"post_project_event_type,omitempty"`
	PostResultStatuses  string `json:"post_result_statuses,omitempty"`
	PostResultExitCodes string `json:"post_result_exit_codes,omitempty"`
	PostContinueOnError bool   `json:"post_continue_on_error"`
	PostTimeoutSeconds  int64  `json:"post_timeout_seconds,omitempty"`
	PostActionsCount    int    `json:"post_actions_count"`
	ContinueOnError     bool   `json:"continue_on_error"`
	TimeoutSeconds      int64  `json:"timeout_seconds,omitempty"`
}

type selectOption struct {
	Value string
	Label string
}

type boolish bool

func (b *boolish) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*b = false
		return nil
	}
	var asBool bool
	if err := json.Unmarshal(data, &asBool); err == nil {
		*b = boolish(asBool)
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(asString)) {
	case "1", "true", "on", "yes", "checked":
		*b = true
	default:
		*b = false
	}
	return nil
}

func (b boolish) Bool() bool { return bool(b) }

type intish int64

func (i *intish) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*i = 0
		return nil
	}
	var asInt int64
	if err := json.Unmarshal(data, &asInt); err == nil {
		*i = intish(asInt)
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err != nil {
		return err
	}
	asString = strings.TrimSpace(asString)
	if asString == "" {
		*i = 0
		return nil
	}
	parsed, err := strconv.ParseInt(asString, 10, 64)
	if err != nil {
		return err
	}
	*i = intish(parsed)
	return nil
}

func (i intish) Int64() int64 { return int64(i) }

type workflowActionResult struct {
	ExitCode   int
	StatusCode int
	Headers    map[string][]string
	Body       string
	Output     string
}

func normalizeActionType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", WorkflowActionLegacyCommand, WorkflowActionRunCommand:
		return WorkflowActionRunCommand
	case WorkflowActionHTTPRequest:
		return WorkflowActionHTTPRequest
	case WorkflowActionProjectMessage:
		return WorkflowActionProjectMessage
	case WorkflowActionTriggerProjectEvent:
		return WorkflowActionTriggerProjectEvent
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func workflowActionOptions() []selectOption {
	return []selectOption{
		{Value: WorkflowActionRunCommand, Label: "Run Command"},
		{Value: WorkflowActionHTTPRequest, Label: "Send HTTP Request"},
		{Value: WorkflowActionProjectMessage, Label: "Send Project Message"},
		{Value: WorkflowActionTriggerProjectEvent, Label: "Trigger Event on Project"},
	}
}

func workflowPostActionOptions() []selectOption {
	return append([]selectOption{{Value: "", Label: "No Post Action"}}, workflowActionOptions()...)
}

func workflowResponseOptions() []selectOption {
	return []selectOption{
		{Value: ResponseModeNoop, Label: "Noop"},
		{Value: ResponseModeSendData, Label: "Send Data"},
		{Value: ResponseModeIterateData, Label: "Iterate Data"},
	}
}

func workflowHTTPMethodOptions() []selectOption {
	return []selectOption{
		{Value: http.MethodGet, Label: http.MethodGet},
		{Value: http.MethodPost, Label: http.MethodPost},
		{Value: http.MethodPut, Label: http.MethodPut},
		{Value: http.MethodPatch, Label: http.MethodPatch},
		{Value: http.MethodDelete, Label: http.MethodDelete},
	}
}

func workflowEventOptionsFromStableEvents(stableEvents []StableEventDefinition) []selectOption {
	options := stableEventOptionsFromDefinitions(stableEvents)
	for event, actions := range GitHubWebhookEvents {
		if len(actions) == 0 {
			options = append(options, selectOption{Value: event, Label: event})
			continue
		}
		sorted := append([]string(nil), actions...)
		sort.Strings(sorted)
		for _, action := range sorted {
			key := event + "." + action
			options = append(options, selectOption{Value: key, Label: key})
		}
	}
	options = append(options, selectOption{Value: "comms", Label: "comms"})
	sort.Slice(options, func(i, j int) bool { return options[i].Value < options[j].Value })
	return options
}

func workflowStepConfigFromNode(node WorkflowNode) workflowStepConfig {
	cfg := workflowActionConfigFromNode(node)
	stepType := normalizeActionType(node.Type)
	step := workflowStepConfig{
		NodeKey:          node.NodeKey,
		Name:             node.Name,
		Type:             stepType,
		TypeLabel:        workflowActionLabel(stepType),
		Command:          node.Command,
		ResponseMode:     responseModeOrDefault(cfg.Response.Mode),
		ResponsePath:     cfg.Response.Path,
		ResponseCapture:  firstNonEmpty(cfg.Response.Capture, node.Capture),
		ContinueOnError:  node.ContinueOnError,
		TimeoutSeconds:   node.TimeoutSeconds,
		PostActionsCount: len(cfg.PostActions),
	}
	if len(cfg.PostActions) > 0 {
		applyPostActionToStepConfig(&step, cfg.PostActions[0])
	}

	switch stepType {
	case WorkflowActionRunCommand:
		step.ResultExitCodes = firstNonEmpty(cfg.Result.SuccessExitCodes, "0")
	case WorkflowActionHTTPRequest:
		step.ResultStatuses = firstNonEmpty(cfg.Result.SuccessStatuses, "200-299")
		if cfg.HTTPRequest != nil {
			step.HTTPMethod = firstNonEmpty(cfg.HTTPRequest.Method, http.MethodGet)
			step.HTTPURL = cfg.HTTPRequest.URL
			step.HTTPHeaders = headersJSONString(cfg.HTTPRequest.Headers)
			step.HTTPBody = cfg.HTTPRequest.Body
		}
	case WorkflowActionProjectMessage:
		if cfg.ProjectMessage != nil {
			step.ProjectRepo = cfg.ProjectMessage.Repo
			step.ProjectRef = cfg.ProjectMessage.Ref
			step.ProjectMessage = cfg.ProjectMessage.Message
		}
	case WorkflowActionTriggerProjectEvent:
		if cfg.ProjectEvent != nil {
			step.ProjectRepo = cfg.ProjectEvent.Repo
			step.ProjectRef = cfg.ProjectEvent.Ref
			step.ProjectEvent = cfg.ProjectEvent.EventType
			step.ProjectMessage = cfg.ProjectEvent.Message
		}
	}

	step.Summary = workflowStepSummary(step)
	return step
}

func applyPostActionToStepConfig(step *workflowStepConfig, post workflowPostActionConfig) {
	step.PostActionName = post.Name
	step.PostActionType = normalizeActionType(post.Type)
	step.PostCommand = post.Command
	step.PostContinueOnError = post.ContinueOnError
	step.PostTimeoutSeconds = post.TimeoutSeconds
	switch step.PostActionType {
	case WorkflowActionRunCommand:
		step.PostResultExitCodes = firstNonEmpty(post.Result.SuccessExitCodes, "0")
	case WorkflowActionHTTPRequest:
		step.PostResultStatuses = firstNonEmpty(post.Result.SuccessStatuses, "200-299")
		if post.HTTPRequest != nil {
			step.PostHTTPMethod = firstNonEmpty(post.HTTPRequest.Method, http.MethodGet)
			step.PostHTTPURL = post.HTTPRequest.URL
			step.PostHTTPHeaders = headersJSONString(post.HTTPRequest.Headers)
			step.PostHTTPBody = post.HTTPRequest.Body
		}
	case WorkflowActionProjectMessage:
		if post.ProjectMessage != nil {
			step.PostProjectRepo = post.ProjectMessage.Repo
			step.PostProjectRef = post.ProjectMessage.Ref
			step.PostProjectMsg = post.ProjectMessage.Message
		}
	case WorkflowActionTriggerProjectEvent:
		if post.ProjectEvent != nil {
			step.PostProjectRepo = post.ProjectEvent.Repo
			step.PostProjectRef = post.ProjectEvent.Ref
			step.PostProjectMsg = post.ProjectEvent.Message
			step.PostProjectEvent = post.ProjectEvent.EventType
		}
	}
}

func workflowActionLabel(actionType string) string {
	for _, option := range workflowActionOptions() {
		if option.Value == actionType {
			return option.Label
		}
	}
	return actionType
}

func workflowStepSummary(step workflowStepConfig) string {
	switch step.Type {
	case WorkflowActionRunCommand:
		return strings.TrimSpace(step.Command)
	case WorkflowActionHTTPRequest:
		return strings.TrimSpace(firstNonEmpty(step.HTTPMethod, http.MethodGet) + " " + step.HTTPURL)
	case WorkflowActionProjectMessage:
		return strings.TrimSpace("comms -> " + step.ProjectRepo)
	case WorkflowActionTriggerProjectEvent:
		return strings.TrimSpace(firstNonEmpty(step.ProjectEvent, "event") + " -> " + step.ProjectRepo)
	default:
		return step.Type
	}
}

func workflowActionConfigFromNode(node WorkflowNode) workflowActionConfig {
	var cfg workflowActionConfig
	if strings.TrimSpace(node.Config) != "" {
		_ = json.Unmarshal([]byte(node.Config), &cfg)
	}
	if cfg.Response.Mode == "" {
		cfg.Response.Mode = ResponseModeNoop
	}
	if cfg.Response.Capture == "" {
		cfg.Response.Capture = node.Capture
	}
	return cfg
}

func workflowNodeFromStepRequest(req workflowStepRequest, index int, existing *WorkflowNode) (WorkflowNode, string) {
	node := WorkflowNode{
		NodeKey:         strings.TrimSpace(req.NodeKey),
		Name:            strings.TrimSpace(firstNonEmpty(req.StepName, req.Name)),
		Type:            normalizeActionType(firstNonEmpty(req.ActionType, req.Type)),
		Command:         strings.TrimSpace(req.Command),
		ContinueOnError: req.ContinueOnError.Bool(),
		TimeoutSeconds:  req.TimeoutSeconds.Int64(),
		PosX:            float64((index - 1) * 220),
		PosY:            0,
	}
	if existing != nil {
		node.ID = existing.ID
		if node.NodeKey == "" {
			node.NodeKey = existing.NodeKey
		}
		if node.Type == "" {
			node.Type = normalizeActionType(existing.Type)
		}
		node.PosX = existing.PosX
		node.PosY = existing.PosY
	}
	if node.NodeKey == "" {
		node.NodeKey = fmt.Sprintf("step-%d", index)
	}
	if node.Type == "" {
		node.Type = WorkflowActionRunCommand
	}

	cfg := workflowActionConfig{
		Result: resultEvaluationConfig{
			SuccessStatuses:  strings.TrimSpace(req.ResultStatuses),
			SuccessExitCodes: strings.TrimSpace(req.ResultExitCodes),
		},
		Response: responseHandlingConfig{
			Mode:    responseModeOrDefault(req.ResponseMode),
			Path:    strings.TrimSpace(req.ResponsePath),
			Capture: strings.TrimSpace(req.ResponseCapture),
		},
	}

	switch node.Type {
	case WorkflowActionRunCommand:
		if node.Command == "" {
			return WorkflowNode{}, "run command steps require a command"
		}
		if cfg.Result.SuccessExitCodes == "" {
			cfg.Result.SuccessExitCodes = "0"
		}
	case WorkflowActionHTTPRequest:
		headers, err := parseWorkflowHeaders(req.HTTPHeaders)
		if err != nil {
			return WorkflowNode{}, "http headers must be a JSON object or Name: Value lines"
		}
		method := strings.ToUpper(firstNonEmpty(req.HTTPMethod, http.MethodGet))
		cfg.HTTPRequest = &httpRequestActionConfig{
			Method:  method,
			URL:     strings.TrimSpace(req.HTTPURL),
			Headers: headers,
			Body:    req.HTTPBody,
		}
		if cfg.Result.SuccessStatuses == "" {
			cfg.Result.SuccessStatuses = "200-299"
		}
	case WorkflowActionProjectMessage:
		cfg.ProjectMessage = &projectMessageActionConfig{
			Repo:    strings.TrimSpace(req.ProjectRepo),
			Ref:     strings.TrimSpace(req.ProjectRef),
			Message: strings.TrimSpace(req.ProjectMessage),
			Sender:  "eventic-workflow",
		}
	case WorkflowActionTriggerProjectEvent:
		cfg.ProjectEvent = &projectEventActionConfig{
			Repo:      strings.TrimSpace(req.ProjectRepo),
			Ref:       strings.TrimSpace(req.ProjectRef),
			EventType: strings.TrimSpace(req.ProjectEvent),
			Message:   strings.TrimSpace(req.ProjectMessage),
			Sender:    "eventic-workflow",
		}
	default:
		return WorkflowNode{}, fmt.Sprintf("unsupported action type %q", node.Type)
	}

	postActions, msg := workflowPostActionsFromStepRequest(req)
	if msg != "" {
		return WorkflowNode{}, msg
	}
	cfg.PostActions = postActions

	node.Capture = cfg.Response.Capture
	data, err := json.Marshal(cfg)
	if err != nil {
		return WorkflowNode{}, "invalid step configuration"
	}
	node.Config = string(data)
	if node.Name == "" {
		node.Name = workflowActionLabel(node.Type)
	}
	if err := validateWorkflowNode(node); err != nil {
		return WorkflowNode{}, err.Error()
	}
	return node, ""
}

func workflowPostActionsFromStepRequest(req workflowStepRequest) ([]workflowPostActionConfig, string) {
	if strings.TrimSpace(firstNonEmpty(
		req.PostActionType,
		req.PostCommand,
		req.PostHTTPURL,
		req.PostProjectRepo,
		req.PostProjectMsg,
		req.PostProjectEvent,
	)) == "" {
		return nil, ""
	}

	actionType := normalizeActionType(req.PostActionType)
	post := workflowPostActionConfig{
		Name:            strings.TrimSpace(req.PostActionName),
		Type:            actionType,
		Command:         strings.TrimSpace(req.PostCommand),
		ContinueOnError: req.PostContinueOnError.Bool(),
		TimeoutSeconds:  req.PostTimeoutSeconds.Int64(),
		Result: resultEvaluationConfig{
			SuccessStatuses:  strings.TrimSpace(req.PostResultStatuses),
			SuccessExitCodes: strings.TrimSpace(req.PostResultExitCodes),
		},
	}

	switch actionType {
	case WorkflowActionRunCommand:
		if post.Command == "" {
			return nil, "post action run command requires a command"
		}
		if post.Result.SuccessExitCodes == "" {
			post.Result.SuccessExitCodes = "0"
		}
	case WorkflowActionHTTPRequest:
		headers, err := parseWorkflowHeaders(req.PostHTTPHeaders)
		if err != nil {
			return nil, "post action http headers must be a JSON object or Name: Value lines"
		}
		post.HTTPRequest = &httpRequestActionConfig{
			Method:  strings.ToUpper(firstNonEmpty(req.PostHTTPMethod, http.MethodPost)),
			URL:     strings.TrimSpace(req.PostHTTPURL),
			Headers: headers,
			Body:    req.PostHTTPBody,
		}
		if post.Result.SuccessStatuses == "" {
			post.Result.SuccessStatuses = "200-299"
		}
	case WorkflowActionProjectMessage:
		post.ProjectMessage = &projectMessageActionConfig{
			Repo:    strings.TrimSpace(req.PostProjectRepo),
			Ref:     strings.TrimSpace(req.PostProjectRef),
			Message: strings.TrimSpace(req.PostProjectMsg),
			Sender:  "eventic-workflow",
		}
	case WorkflowActionTriggerProjectEvent:
		post.ProjectEvent = &projectEventActionConfig{
			Repo:      strings.TrimSpace(req.PostProjectRepo),
			Ref:       strings.TrimSpace(req.PostProjectRef),
			EventType: strings.TrimSpace(req.PostProjectEvent),
			Message:   strings.TrimSpace(req.PostProjectMsg),
			Sender:    "eventic-workflow",
		}
	default:
		return nil, fmt.Sprintf("unsupported post action type %q", actionType)
	}
	return []workflowPostActionConfig{post}, ""
}

func validateWorkflowNode(node WorkflowNode) error {
	node.Type = normalizeActionType(node.Type)
	cfg := workflowActionConfigFromNode(node)
	if cfg.Response.Mode == "" {
		cfg.Response.Mode = ResponseModeNoop
	}
	switch cfg.Response.Mode {
	case ResponseModeNoop:
	case ResponseModeSendData:
		if len(cfg.PostActions) == 0 && strings.TrimSpace(firstNonEmpty(cfg.Response.Capture, node.Capture)) == "" {
			return fmt.Errorf("node %q response send data requires a capture variable", node.NodeKey)
		}
	case ResponseModeIterateData:
		if strings.TrimSpace(cfg.Response.Path) == "" {
			return fmt.Errorf("node %q iterate data requires a response path", node.NodeKey)
		}
		if len(cfg.PostActions) == 0 && strings.TrimSpace(firstNonEmpty(cfg.Response.Capture, node.Capture)) == "" {
			return fmt.Errorf("node %q iterate data requires a capture variable", node.NodeKey)
		}
	default:
		return fmt.Errorf("node %q has unsupported response mode %q", node.NodeKey, cfg.Response.Mode)
	}

	switch node.Type {
	case WorkflowActionRunCommand:
		if strings.TrimSpace(node.Command) == "" {
			return fmt.Errorf("node %q requires a command", node.NodeKey)
		}
		if cfg.Result.SuccessExitCodes != "" {
			if _, err := parseNumberMatchers(cfg.Result.SuccessExitCodes); err != nil {
				return fmt.Errorf("node %q has invalid success exit codes: %w", node.NodeKey, err)
			}
		}
	case WorkflowActionHTTPRequest:
		if cfg.HTTPRequest == nil {
			return fmt.Errorf("node %q requires http_request config", node.NodeKey)
		}
		method := strings.ToUpper(strings.TrimSpace(cfg.HTTPRequest.Method))
		if method == "" {
			method = http.MethodGet
		}
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return fmt.Errorf("node %q has unsupported http method %q", node.NodeKey, method)
		}
		if strings.TrimSpace(cfg.HTTPRequest.URL) == "" {
			return fmt.Errorf("node %q requires an http url", node.NodeKey)
		}
		if _, err := url.ParseRequestURI(cfg.HTTPRequest.URL); err != nil {
			return fmt.Errorf("node %q has invalid http url", node.NodeKey)
		}
		if cfg.Result.SuccessStatuses != "" {
			if _, err := parseNumberMatchers(cfg.Result.SuccessStatuses); err != nil {
				return fmt.Errorf("node %q has invalid success statuses: %w", node.NodeKey, err)
			}
		}
	case WorkflowActionProjectMessage:
		if cfg.ProjectMessage == nil {
			return fmt.Errorf("node %q requires project_message config", node.NodeKey)
		}
		if err := validateRepoName(cfg.ProjectMessage.Repo); err != nil {
			return fmt.Errorf("node %q project repo %w", node.NodeKey, err)
		}
		if strings.TrimSpace(cfg.ProjectMessage.Message) == "" {
			return fmt.Errorf("node %q requires a project message", node.NodeKey)
		}
	case WorkflowActionTriggerProjectEvent:
		if cfg.ProjectEvent == nil {
			return fmt.Errorf("node %q requires project_event config", node.NodeKey)
		}
		if err := validateRepoName(cfg.ProjectEvent.Repo); err != nil {
			return fmt.Errorf("node %q project repo %w", node.NodeKey, err)
		}
		if strings.TrimSpace(cfg.ProjectEvent.EventType) == "" {
			return fmt.Errorf("node %q requires a project event type", node.NodeKey)
		}
	default:
		return fmt.Errorf("node %q has unsupported action type %q", node.NodeKey, node.Type)
	}

	for i, post := range cfg.PostActions {
		if err := validateWorkflowPostAction(node.NodeKey, i, post); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkflowPostAction(nodeKey string, index int, post workflowPostActionConfig) error {
	post.Type = normalizeActionType(post.Type)
	switch post.Type {
	case WorkflowActionRunCommand:
		if strings.TrimSpace(post.Command) == "" {
			return fmt.Errorf("node %q post action %d requires a command", nodeKey, index+1)
		}
		if post.Result.SuccessExitCodes != "" {
			if _, err := parseNumberMatchers(post.Result.SuccessExitCodes); err != nil {
				return fmt.Errorf("node %q post action %d has invalid success exit codes: %w", nodeKey, index+1, err)
			}
		}
	case WorkflowActionHTTPRequest:
		if post.HTTPRequest == nil {
			return fmt.Errorf("node %q post action %d requires http_request config", nodeKey, index+1)
		}
		method := strings.ToUpper(strings.TrimSpace(post.HTTPRequest.Method))
		if method == "" {
			method = http.MethodPost
		}
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return fmt.Errorf("node %q post action %d has unsupported http method %q", nodeKey, index+1, method)
		}
		if strings.TrimSpace(post.HTTPRequest.URL) == "" {
			return fmt.Errorf("node %q post action %d requires an http url", nodeKey, index+1)
		}
		if _, err := url.ParseRequestURI(post.HTTPRequest.URL); err != nil {
			return fmt.Errorf("node %q post action %d has invalid http url", nodeKey, index+1)
		}
		if post.Result.SuccessStatuses != "" {
			if _, err := parseNumberMatchers(post.Result.SuccessStatuses); err != nil {
				return fmt.Errorf("node %q post action %d has invalid success statuses: %w", nodeKey, index+1, err)
			}
		}
	case WorkflowActionProjectMessage:
		if post.ProjectMessage == nil {
			return fmt.Errorf("node %q post action %d requires project_message config", nodeKey, index+1)
		}
		if err := validateRepoName(post.ProjectMessage.Repo); err != nil {
			return fmt.Errorf("node %q post action %d project repo %w", nodeKey, index+1, err)
		}
		if strings.TrimSpace(post.ProjectMessage.Message) == "" {
			return fmt.Errorf("node %q post action %d requires a project message", nodeKey, index+1)
		}
	case WorkflowActionTriggerProjectEvent:
		if post.ProjectEvent == nil {
			return fmt.Errorf("node %q post action %d requires project_event config", nodeKey, index+1)
		}
		if err := validateRepoName(post.ProjectEvent.Repo); err != nil {
			return fmt.Errorf("node %q post action %d project repo %w", nodeKey, index+1, err)
		}
		if strings.TrimSpace(post.ProjectEvent.EventType) == "" {
			return fmt.Errorf("node %q post action %d requires a project event type", nodeKey, index+1)
		}
	default:
		return fmt.Errorf("node %q post action %d has unsupported action type %q", nodeKey, index+1, post.Type)
	}
	return nil
}

func runWorkflowAction(ctx context.Context, repoPath string, event protocol.EventMsg, step DAGStep, rc *RunContext, replay ReplayDispatcher) NodeResult {
	actionType := normalizeActionType(step.Type)
	cfg := workflowActionConfigFromStep(step)

	action, err := executeWorkflowAction(ctx, repoPath, event, step, rc, cfg, replay)
	if err != nil {
		return NodeResult{Key: step.Key, State: NodeStateFailure, ExitCode: 1, Output: trimString(err.Error(), maxNodeOutputBytes)}
	}

	if !actionStatusSucceeded(actionType, cfg, action) {
		output := action.Output
		if output == "" {
			output = action.Body
		}
		if actionType == WorkflowActionHTTPRequest {
			output = fmt.Sprintf("http status %d did not match %q\n%s", action.StatusCode, firstNonEmpty(cfg.Result.SuccessStatuses, "200-299"), output)
		} else {
			output = fmt.Sprintf("exit code %d did not match %q\n%s", action.ExitCode, firstNonEmpty(cfg.Result.SuccessExitCodes, "0"), output)
		}
		return NodeResult{Key: step.Key, State: NodeStateFailure, ExitCode: failureExitCode(action), Output: trimString(strings.TrimSpace(output), maxNodeOutputBytes)}
	}

	output, err := actionResponseOutput(ctx, repoPath, event, step, rc, cfg, action, replay)
	if err != nil {
		return NodeResult{Key: step.Key, State: NodeStateFailure, ExitCode: 1, Output: trimString(err.Error(), maxNodeOutputBytes)}
	}
	return NodeResult{Key: step.Key, State: NodeStateSuccess, ExitCode: 0, Output: trimString(strings.TrimSpace(output), maxNodeOutputBytes)}
}

func executeWorkflowAction(ctx context.Context, repoPath string, event protocol.EventMsg, step DAGStep, rc *RunContext, cfg workflowActionConfig, replay ReplayDispatcher) (workflowActionResult, error) {
	actionType := normalizeActionType(step.Type)
	var action workflowActionResult
	var err error
	switch actionType {
	case WorkflowActionRunCommand:
		action, err = runCommandWorkflowAction(ctx, repoPath, event, step, rc, cfg)
	case WorkflowActionHTTPRequest:
		action, err = runHTTPWorkflowAction(ctx, event, step, rc, cfg)
	case WorkflowActionProjectMessage:
		action, err = dispatchProjectMessageAction(ctx, event, step, rc, cfg, replay)
	case WorkflowActionTriggerProjectEvent:
		action, err = dispatchProjectEventAction(ctx, event, step, rc, cfg, replay)
	default:
		err = fmt.Errorf("unsupported action type %q", actionType)
	}
	return action, err
}

func workflowActionConfigFromStep(step DAGStep) workflowActionConfig {
	var cfg workflowActionConfig
	if strings.TrimSpace(step.Config) != "" {
		_ = json.Unmarshal([]byte(step.Config), &cfg)
	}
	if cfg.Response.Mode == "" {
		cfg.Response.Mode = ResponseModeNoop
	}
	if cfg.Response.Capture == "" {
		cfg.Response.Capture = step.Capture
	}
	return cfg
}

func runCommandWorkflowAction(ctx context.Context, repoPath string, event protocol.EventMsg, step DAGStep, rc *RunContext, _ workflowActionConfig) (workflowActionResult, error) {
	extra := []string{"EVENTIC_CONTEXT_DIR=" + rc.WorkspaceDir}
	for name, value := range rc.Vars {
		extra = append(extra, name+"="+value)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", step.Command)
	cmd.Dir = repoPath
	if len(rc.BaseEnv) > 0 {
		cmd.Env = append(append([]string{}, rc.BaseEnv...), buildHookEnv(repoPath, event, extra...)...)
	} else {
		cmd.Env = buildHookEnv(repoPath, event, extra...)
	}

	out, err := cmd.CombinedOutput()
	output := trimString(strings.TrimSpace(string(out)), maxNodeOutputBytes)
	return workflowActionResult{
		ExitCode: exitCodeFromError(err),
		Output:   output,
		Body:     output,
	}, nil
}

func runHTTPWorkflowAction(ctx context.Context, event protocol.EventMsg, step DAGStep, rc *RunContext, cfg workflowActionConfig) (workflowActionResult, error) {
	if cfg.HTTPRequest == nil {
		return workflowActionResult{}, fmt.Errorf("node %q missing http request config", step.Key)
	}
	method := strings.ToUpper(firstNonEmpty(expandWorkflowValue(cfg.HTTPRequest.Method, event, rc), http.MethodGet))
	rawURL := expandWorkflowValue(cfg.HTTPRequest.URL, event, rc)
	body := expandWorkflowValue(cfg.HTTPRequest.Body, event, rc)

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewBufferString(body))
	if err != nil {
		return workflowActionResult{}, err
	}
	for name, value := range cfg.HTTPRequest.Headers {
		req.Header.Set(name, expandWorkflowValue(value, event, rc))
	}
	if req.Header.Get("Content-Type") == "" && body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return workflowActionResult{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNodeOutputBytes+1))
	if err != nil {
		return workflowActionResult{}, err
	}
	bodyText := trimString(strings.TrimSpace(string(data)), maxNodeOutputBytes)
	return workflowActionResult{
		StatusCode: resp.StatusCode,
		Headers:    map[string][]string(resp.Header),
		Body:       bodyText,
		Output:     bodyText,
	}, nil
}

func dispatchProjectMessageAction(ctx context.Context, event protocol.EventMsg, step DAGStep, rc *RunContext, cfg workflowActionConfig, replay ReplayDispatcher) (workflowActionResult, error) {
	if replay == nil {
		return workflowActionResult{}, fmt.Errorf("node %q cannot send project message: replay dispatcher unavailable", step.Key)
	}
	if cfg.ProjectMessage == nil {
		return workflowActionResult{}, fmt.Errorf("node %q missing project message config", step.Key)
	}
	target := cfg.ProjectMessage
	deliveryID := fmt.Sprintf("workflow-%d", time.Now().UnixNano())
	message := expandWorkflowValue(target.Message, event, rc)
	payload := map[string]any{
		"repo":     expandWorkflowValue(target.Repo, event, rc),
		"ref":      expandWorkflowValue(firstNonEmpty(target.Ref, event.Ref), event, rc),
		"message":  message,
		"sender":   firstNonEmpty(target.Sender, "eventic-workflow"),
		"workflow": step.Key,
	}
	body, _ := json.Marshal(payload)
	replay(ctx, protocol.EventMsg{
		MsgType:     "Event",
		DeliveryID:  deliveryID,
		GitHubEvent: "comms",
		Repo:        payload["repo"].(string),
		Ref:         payload["ref"].(string),
		Sender:      payload["sender"].(string),
		Message:     message,
		CloneURL:    firstNonEmpty(target.CloneURL, "https://github.com/"+payload["repo"].(string)+".git"),
		Payload:     body,
	})
	out := fmt.Sprintf(`{"status":"accepted","delivery_id":%q,"repo":%q,"event_type":"comms"}`, deliveryID, payload["repo"].(string))
	return workflowActionResult{ExitCode: 0, Output: out, Body: out}, nil
}

func dispatchProjectEventAction(ctx context.Context, event protocol.EventMsg, step DAGStep, rc *RunContext, cfg workflowActionConfig, replay ReplayDispatcher) (workflowActionResult, error) {
	if replay == nil {
		return workflowActionResult{}, fmt.Errorf("node %q cannot trigger project event: replay dispatcher unavailable", step.Key)
	}
	if cfg.ProjectEvent == nil {
		return workflowActionResult{}, fmt.Errorf("node %q missing project event config", step.Key)
	}
	target := cfg.ProjectEvent
	eventType := expandWorkflowValue(target.EventType, event, rc)
	eventName, action := splitEventType(eventType)
	repo := expandWorkflowValue(target.Repo, event, rc)
	ref := expandWorkflowValue(firstNonEmpty(target.Ref, event.Ref), event, rc)
	deliveryID := fmt.Sprintf("workflow-%d", time.Now().UnixNano())
	message := expandWorkflowValue(target.Message, event, rc)
	payload := replayPayload(repo, ref, eventName, action, deliveryID)
	replayEvent := protocol.EventMsg{
		MsgType:     "Event",
		DeliveryID:  deliveryID,
		GitHubEvent: eventName,
		Action:      action,
		Repo:        repo,
		Ref:         ref,
		Sender:      firstNonEmpty(target.Sender, "eventic-workflow"),
		Message:     message,
		CloneURL:    firstNonEmpty(target.CloneURL, "https://github.com/"+repo+".git"),
		Payload:     payload,
	}
	if isStableInternalEvent(eventType) {
		replayEvent.Provider = "eventic"
		replayEvent.StableEvent = eventType
		replayEvent.ExternalEvent = "workflow"
		replayEvent.ExternalAction = step.Key
		replayEvent.GitHubEvent = "internal"
		replayEvent.Action = ""
	}
	replay(ctx, replayEvent)
	out := fmt.Sprintf(`{"status":"accepted","delivery_id":%q,"repo":%q,"event_type":%q}`, deliveryID, repo, eventType)
	return workflowActionResult{ExitCode: 0, Output: out, Body: out}, nil
}

func actionStatusSucceeded(actionType string, cfg workflowActionConfig, result workflowActionResult) bool {
	switch actionType {
	case WorkflowActionHTTPRequest:
		return numberMatches(firstNonEmpty(cfg.Result.SuccessStatuses, "200-299"), result.StatusCode)
	default:
		return numberMatches(firstNonEmpty(cfg.Result.SuccessExitCodes, "0"), result.ExitCode)
	}
}

func failureExitCode(result workflowActionResult) int {
	if result.ExitCode != 0 {
		return result.ExitCode
	}
	return 1
}

func actionResponseOutput(ctx context.Context, repoPath string, event protocol.EventMsg, step DAGStep, rc *RunContext, cfg workflowActionConfig, result workflowActionResult, replay ReplayDispatcher) (string, error) {
	mode := responseModeOrDefault(cfg.Response.Mode)
	switch mode {
	case ResponseModeNoop:
		if len(cfg.PostActions) > 0 {
			return runWorkflowPostActions(ctx, repoPath, event, step, rc, cfg, replay, []postActionPayload{{
				Data:  firstNonEmpty(result.Output, result.Body),
				Index: -1,
			}})
		}
		return firstNonEmpty(result.Output, result.Body), nil
	case ResponseModeSendData:
		value, err := selectActionResponseValue(result, cfg.Response.Path)
		if err != nil {
			return "", err
		}
		data := stringifyResponseValue(value)
		if len(cfg.PostActions) > 0 {
			return runWorkflowPostActions(ctx, repoPath, event, step, rc, cfg, replay, []postActionPayload{{Data: data, Index: -1}})
		}
		return data, nil
	case ResponseModeIterateData:
		value, err := selectActionResponseValue(result, cfg.Response.Path)
		if err != nil {
			return "", err
		}
		items, ok := value.([]any)
		if !ok {
			return "", fmt.Errorf("response path %q did not resolve to an array", cfg.Response.Path)
		}
		payloads := make([]postActionPayload, 0, len(items))
		for i, item := range items {
			payloads = append(payloads, postActionPayload{Data: stringifyResponseValue(item), Index: i})
		}
		if len(cfg.PostActions) > 0 {
			return runWorkflowPostActions(ctx, repoPath, event, step, rc, cfg, replay, payloads)
		}
		lines := make([]string, 0, len(payloads))
		for _, payload := range payloads {
			lines = append(lines, payload.Data)
		}
		return strings.Join(lines, "\n"), nil
	default:
		return "", fmt.Errorf("unsupported response mode %q", mode)
	}
}

type postActionPayload struct {
	Data  string
	Index int
}

func runWorkflowPostActions(ctx context.Context, repoPath string, event protocol.EventMsg, step DAGStep, rc *RunContext, cfg workflowActionConfig, replay ReplayDispatcher, payloads []postActionPayload) (string, error) {
	if len(payloads) == 0 {
		return "", nil
	}
	var outputs []string
	for _, payload := range payloads {
		runCtx := runContextWithResponseData(rc, cfg, payload)
		for i, post := range cfg.PostActions {
			output, err := runWorkflowPostAction(ctx, repoPath, event, step, runCtx, post, i, replay)
			if output != "" {
				outputs = append(outputs, output)
			}
			if err != nil {
				if post.ContinueOnError {
					outputs = append(outputs, err.Error())
					continue
				}
				return strings.Join(outputs, "\n"), err
			}
		}
	}
	return strings.Join(outputs, "\n"), nil
}

func runWorkflowPostAction(ctx context.Context, repoPath string, event protocol.EventMsg, parent DAGStep, rc *RunContext, post workflowPostActionConfig, index int, replay ReplayDispatcher) (string, error) {
	step := workflowPostActionStep(parent, post, index)
	cfg := workflowActionConfig{
		HTTPRequest:    post.HTTPRequest,
		ProjectMessage: post.ProjectMessage,
		ProjectEvent:   post.ProjectEvent,
		Result:         post.Result,
		Response:       responseHandlingConfig{Mode: ResponseModeNoop},
	}
	nodeCtx := ctx
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		nodeCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}
	result, err := executeWorkflowAction(nodeCtx, repoPath, event, step, rc, cfg, replay)
	if err != nil {
		return "", err
	}
	actionType := normalizeActionType(step.Type)
	if !actionStatusSucceeded(actionType, cfg, result) {
		output := firstNonEmpty(result.Output, result.Body)
		if actionType == WorkflowActionHTTPRequest {
			return output, fmt.Errorf("post action %q http status %d did not match %q", step.Key, result.StatusCode, firstNonEmpty(cfg.Result.SuccessStatuses, "200-299"))
		}
		return output, fmt.Errorf("post action %q exit code %d did not match %q", step.Key, result.ExitCode, firstNonEmpty(cfg.Result.SuccessExitCodes, "0"))
	}
	return firstNonEmpty(result.Output, result.Body), nil
}

func workflowPostActionStep(parent DAGStep, post workflowPostActionConfig, index int) DAGStep {
	actionType := normalizeActionType(post.Type)
	key := fmt.Sprintf("%s.post-%d", parent.Key, index+1)
	name := post.Name
	if strings.TrimSpace(name) == "" {
		name = workflowActionLabel(actionType)
	}
	return DAGStep{
		Key:             key,
		Name:            name,
		Type:            actionType,
		Command:         post.Command,
		ContinueOnError: post.ContinueOnError,
		Timeout:         time.Duration(post.TimeoutSeconds) * time.Second,
	}
}

func runContextWithResponseData(rc *RunContext, cfg workflowActionConfig, payload postActionPayload) *RunContext {
	vars := make(map[string]string)
	if rc != nil {
		for key, value := range rc.Vars {
			vars[key] = value
		}
	}
	vars["EVENTIC_RESPONSE_DATA"] = payload.Data
	if payload.Index >= 0 {
		vars["EVENTIC_ITEM"] = payload.Data
		vars["EVENTIC_ITEM_INDEX"] = strconv.Itoa(payload.Index)
	}
	if capture := strings.TrimSpace(cfg.Response.Capture); capture != "" {
		vars[capture] = payload.Data
	}
	next := &RunContext{Vars: vars}
	if rc != nil {
		next.WorkspaceDir = rc.WorkspaceDir
		next.BaseEnv = rc.BaseEnv
	}
	return next
}

func selectActionResponseValue(result workflowActionResult, path string) (any, error) {
	body := any(result.Body)
	if result.Body != "" {
		var parsed any
		if json.Unmarshal([]byte(result.Body), &parsed) == nil {
			body = parsed
		}
	}
	root := map[string]any{
		"status_code": result.StatusCode,
		"exit_code":   result.ExitCode,
		"headers":     result.Headers,
		"body":        body,
		"output":      firstNonEmpty(result.Output, result.Body),
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return root["output"], nil
	}
	value := any(root)
	for _, part := range strings.Split(path, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("invalid empty response path segment")
		}
		switch typed := value.(type) {
		case map[string]any:
			next, ok := typed[part]
			if !ok {
				return nil, fmt.Errorf("response path %q not found", path)
			}
			value = next
		case map[string][]string:
			next, ok := typed[part]
			if !ok {
				return nil, fmt.Errorf("response path %q not found", path)
			}
			value = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("response path %q has invalid array index %q", path, part)
			}
			value = typed[index]
		default:
			return nil, fmt.Errorf("response path %q cannot descend into %T", path, value)
		}
	}
	return value, nil
}

func stringifyResponseValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(data)
	}
}

type numberMatcher struct {
	start int
	end   int
}

func numberMatches(spec string, value int) bool {
	matchers, err := parseNumberMatchers(spec)
	if err != nil {
		return false
	}
	for _, matcher := range matchers {
		if value >= matcher.start && value <= matcher.end {
			return true
		}
	}
	return false
}

func parseNumberMatchers(spec string) ([]numberMatcher, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty matcher")
	}
	parts := strings.Split(spec, ",")
	matchers := make([]numberMatcher, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			ends := strings.SplitN(part, "-", 2)
			start, err := strconv.Atoi(strings.TrimSpace(ends[0]))
			if err != nil {
				return nil, err
			}
			end, err := strconv.Atoi(strings.TrimSpace(ends[1]))
			if err != nil {
				return nil, err
			}
			if end < start {
				return nil, fmt.Errorf("range %q ends before it starts", part)
			}
			matchers = append(matchers, numberMatcher{start: start, end: end})
			continue
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, numberMatcher{start: value, end: value})
	}
	if len(matchers) == 0 {
		return nil, fmt.Errorf("empty matcher")
	}
	return matchers, nil
}

func parseWorkflowHeaders(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	headers := map[string]string{}
	if strings.HasPrefix(raw, "{") {
		if err := json.Unmarshal([]byte(raw), &headers); err != nil {
			return nil, err
		}
		return headers, nil
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid header line %q", line)
		}
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return headers, nil
}

func headersJSONString(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	data, err := json.Marshal(headers)
	if err != nil {
		return ""
	}
	return string(data)
}

func responseModeOrDefault(mode string) string {
	switch strings.TrimSpace(mode) {
	case "", ResponseModeNoop:
		return ResponseModeNoop
	case ResponseModeSendData:
		return ResponseModeSendData
	case ResponseModeIterateData:
		return ResponseModeIterateData
	default:
		return strings.TrimSpace(mode)
	}
}

func validateRepoName(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fmt.Errorf("is required")
	}
	if strings.Count(repo, "/") != 1 || strings.Contains(repo, "..") {
		return fmt.Errorf("must be in org/repo form")
	}
	return nil
}

func expandWorkflowValue(value string, event protocol.EventMsg, rc *RunContext) string {
	return os.Expand(value, func(name string) string {
		if rc != nil && rc.Vars != nil {
			if value, ok := rc.Vars[name]; ok {
				return value
			}
		}
		switch name {
		case "EVENTIC_REPO":
			return event.Repo
		case "EVENTIC_REF":
			return event.Ref
		case "EVENTIC_EVENT":
			return event.GitHubEvent
		case "EVENTIC_ACTION":
			return event.Action
		case "EVENTIC_SENDER":
			return event.Sender
		case "EVENTIC_MESSAGE":
			return event.Message
		case "EVENTIC_DELIVERY_ID":
			return event.DeliveryID
		default:
			return ""
		}
	})
}
