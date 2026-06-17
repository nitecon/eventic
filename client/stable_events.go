package client

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nitecon/eventic/protocol"
)

const (
	StableEventArtifactInitiate      = "artifact.initiate"
	StableEventArtifactPublished     = "artifact.published"
	StableEventSystemFailure         = "system.failure"
	StableEventSecurityAlarm         = "security.alarm"
	StableEventCommunicationReceived = "communication.received"
)

// StableEventDefinition describes one workflow-facing event key. These values
// are intentionally provider-agnostic and are safe for workflow subscriptions.
type StableEventDefinition struct {
	ID             int64     `json:"id,omitempty"`
	Event          string    `json:"event"`
	Title          string    `json:"title"`
	Group          string    `json:"group"`
	Description    string    `json:"description"`
	Enabled        bool      `json:"enabled"`
	BuiltIn        bool      `json:"built_in"`
	ExampleSources []string  `json:"example_sources"`
	WorkflowUses   []string  `json:"workflow_uses"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

// NormalizedEvent is the strict shape produced by the inbound normalization
// layer before a workflow is resolved.
type NormalizedEvent struct {
	ID                  string            `json:"id"`
	Source              string            `json:"source"`
	StableEvent         string            `json:"stable_event"`
	Timestamp           time.Time         `json:"timestamp"`
	Actor               string            `json:"actor,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	RawPayloadReference string            `json:"raw_payload_reference,omitempty"`
}

// StableEventDefinitions returns the canonical stable event catalog shown in
// the dashboard and used by workflow dropdowns.
func StableEventDefinitions() []StableEventDefinition {
	return []StableEventDefinition{
		{
			Event:          StableEventArtifactInitiate,
			Title:          "Initiate Artifact",
			Group:          "Artifact",
			Description:    "Start building or preparing a new artifact.",
			Enabled:        true,
			BuiltIn:        true,
			ExampleSources: []string{"github push refs/heads/main", "gitlab merge_request merged", "bitbucket repo.push"},
			WorkflowUses:   []string{"Build and store an artifact", "Start security scans", "Notify a build channel"},
		},
		{
			Event:          StableEventArtifactPublished,
			Title:          "Publish Artifact",
			Group:          "Artifact",
			Description:    "Promote an existing verified artifact without rebuilding it.",
			Enabled:        true,
			BuiltIn:        true,
			ExampleSources: []string{"github release.published", "github tag push", "bitbucket tag push"},
			WorkflowUses:   []string{"Promote registry tags", "Restart production services", "Start smoke tests"},
		},
		{
			Event:          StableEventSystemFailure,
			Title:          "System Failure",
			Group:          "SRE",
			Description:    "React to a failed build, failed workflow, or operational alarm.",
			Enabled:        true,
			BuiltIn:        true,
			ExampleSources: []string{"github workflow_run.completed failure", "prometheus Alertmanager firing", "datadog monitor alert"},
			WorkflowUses:   []string{"Notify a project channel", "Correlate with recent deploys", "Page on-call or roll back"},
		},
		{
			Event:          StableEventSecurityAlarm,
			Title:          "Security Alarm",
			Group:          "Security",
			Description:    "React to high-risk vulnerabilities, dependency alerts, or registry scan findings.",
			Enabled:        true,
			BuiltIn:        true,
			ExampleSources: []string{"github dependabot_alert.created", "snyk vulnerability", "aws inspector finding"},
			WorkflowUses:   []string{"Mark artifacts tainted", "Open a priority ticket", "Notify security owners"},
		},
		{
			Event:          StableEventCommunicationReceived,
			Title:          "Communication Received",
			Group:          "Communication",
			Description:    "Handle internal application messages or agent instructions.",
			Enabled:        true,
			BuiltIn:        true,
			ExampleSources: []string{"eventic comms", "custom webhook title/body/action", "discord interaction"},
			WorkflowUses:   []string{"Route an agent task", "Post a project update", "Trigger follow-up workflow steps"},
		},
	}
}

func stableEventOptions() []selectOption {
	return stableEventOptionsFromDefinitions(StableEventDefinitions())
}

func stableEventOptionsFromDefinitions(defs []StableEventDefinition) []selectOption {
	options := make([]selectOption, 0, len(defs))
	for _, def := range defs {
		options = append(options, selectOption{Value: def.Event, Label: def.Event})
	}
	return options
}

// NormalizeInboundEvent applies persisted declarative mappings to an event. The
// returned EventMsg keeps the original provider event fields intact and only
// fills StableEvent when a mapping matches or the sender already supplied one.
func NormalizeInboundEvent(ctx context.Context, store *ProjectStore, event protocol.EventMsg) (protocol.EventMsg, NormalizedEvent, error) {
	event.Provider = eventProvider(event)
	if event.ExternalEvent == "" {
		event.ExternalEvent = event.GitHubEvent
	}
	if event.ExternalAction == "" {
		event.ExternalAction = event.Action
	}
	if event.Metadata == nil {
		event.Metadata = eventMetadata(event)
	}

	stableEvent := strings.TrimSpace(event.StableEvent)
	if stableEvent == "" && store != nil {
		mappings, err := store.ListEventMappings(ctx)
		if err != nil {
			return event, NormalizedEvent{}, err
		}
		stableEvent = resolveStableEventFromMappings(event, mappings)
	}
	event.StableEvent = stableEvent

	normalized := NormalizedEvent{
		ID:          event.DeliveryID,
		Source:      event.Provider,
		StableEvent: stableEvent,
		Timestamp:   time.Now().UTC(),
		Actor:       event.Sender,
		Metadata:    event.Metadata,
	}
	if event.DeliveryID != "" && len(event.Payload) > 0 {
		normalized.RawPayloadReference = "eventic://payload/" + event.DeliveryID
	}
	return event, normalized, nil
}

func workflowEventType(event protocol.EventMsg) string {
	if event.StableEvent != "" {
		return event.StableEvent
	}
	return event.GitHubEvent
}

func workflowEventAction(event protocol.EventMsg) string {
	if event.StableEvent != "" {
		return ""
	}
	return event.Action
}

func isStableInternalEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return false
	}
	for _, def := range StableEventDefinitions() {
		if def.Event == eventType {
			return true
		}
	}
	eventName, _ := splitEventType(eventType)
	if _, external := GitHubWebhookEvents[eventName]; external {
		return false
	}
	return eventName != "comms"
}

func eventProvider(event protocol.EventMsg) string {
	if event.Provider != "" {
		return strings.ToLower(strings.TrimSpace(event.Provider))
	}
	switch event.GitHubEvent {
	case "comms":
		return "custom"
	default:
		return "github"
	}
}

func eventMetadata(event protocol.EventMsg) map[string]string {
	metadata := map[string]string{}
	if event.Ref != "" {
		metadata["ref"] = event.Ref
		if strings.HasPrefix(event.Ref, "refs/tags/") {
			metadata["target_environment"] = "production"
		}
	}
	if sha := firstJSONPath(event.Payload, "after", "head_commit.id", "workflow_run.head_sha"); sha != "" {
		metadata["commit_sha"] = sha
	}
	if severity := firstJSONPath(event.Payload,
		"alert.security_advisory.severity",
		"security_advisory.severity",
		"commonLabels.severity",
		"alerts[].labels.severity",
		"vulnerabilities[].severity",
		"severity",
	); severity != "" {
		metadata["severity"] = strings.ToUpper(severity)
	}
	return metadata
}

func resolveStableEventFromMappings(event protocol.EventMsg, mappings []EventMapping) string {
	for _, mapping := range mappings {
		if !mapping.Enabled || strings.TrimSpace(mapping.TargetStableEvent) == "" {
			continue
		}
		if mappingMatchesEvent(mapping, event) {
			return mapping.TargetStableEvent
		}
	}
	return ""
}

func mappingMatchesEvent(mapping EventMapping, event protocol.EventMsg) bool {
	if mapping.Provider != "" && mapping.Provider != "*" && !strings.EqualFold(mapping.Provider, eventProvider(event)) {
		return false
	}
	for key, expected := range mapping.Conditions {
		actuals := conditionValues(event, key)
		if len(actuals) == 0 {
			return false
		}
		matched := false
		for _, actual := range actuals {
			if conditionValueMatches(actual, expected) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func conditionValues(event protocol.EventMsg, key string) []string {
	key = strings.TrimSpace(key)
	switch {
	case key == "provider" || key == "source":
		return []string{eventProvider(event)}
	case key == "event" || key == "external_event":
		return []string{firstNonEmpty(event.ExternalEvent, event.GitHubEvent)}
	case key == "action" || key == "external_action":
		return []string{firstNonEmpty(event.ExternalAction, event.Action)}
	case key == "ref":
		return []string{event.Ref}
	case key == "repo":
		return []string{event.Repo}
	case key == "sender" || key == "actor":
		return []string{event.Sender}
	case key == "message":
		return []string{event.Message}
	case strings.HasPrefix(key, "headers."):
		header := strings.TrimPrefix(key, "headers.")
		if event.Headers == nil {
			return nil
		}
		for name, value := range event.Headers {
			if strings.EqualFold(name, header) {
				return []string{value}
			}
		}
	case strings.HasPrefix(key, "metadata."):
		name := strings.TrimPrefix(key, "metadata.")
		if event.Metadata == nil {
			return nil
		}
		for actualName, value := range event.Metadata {
			if strings.EqualFold(actualName, name) {
				return []string{value}
			}
		}
	case strings.HasPrefix(key, "body."):
		return jsonPathValues(event.Payload, strings.TrimPrefix(key, "body."))
	}
	return nil
}

func conditionValueMatches(actual, expected string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "*" {
		return true
	}
	actual = strings.TrimSpace(actual)
	for _, candidate := range strings.Split(expected, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.EqualFold(actual, candidate) {
			return true
		}
		if strings.ContainsAny(candidate, "*?[") {
			if matched, _ := path.Match(candidate, actual); matched {
				return true
			}
		}
	}
	return false
}

func firstJSONPath(payload json.RawMessage, paths ...string) string {
	for _, path := range paths {
		values := jsonPathValues(payload, path)
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func jsonPathValues(payload json.RawMessage, path string) []string {
	if len(payload) == 0 || strings.TrimSpace(path) == "" {
		return nil
	}
	var body any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil
	}
	nodes := []any{body}
	for _, rawPart := range strings.Split(path, ".") {
		part := strings.TrimSpace(rawPart)
		if part == "" {
			return nil
		}
		nodes = selectJSONPathPart(nodes, part)
		if len(nodes) == 0 {
			return nil
		}
	}

	values := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if value := stringifyJSONValue(node); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func selectJSONPathPart(nodes []any, part string) []any {
	iterateArray := strings.HasSuffix(part, "[]")
	if iterateArray {
		part = strings.TrimSuffix(part, "[]")
	}

	var selected []any
	for _, node := range nodes {
		if part != "" {
			object, ok := node.(map[string]any)
			if !ok {
				continue
			}
			var found bool
			node, found = lookupJSONObject(object, part)
			if !found {
				continue
			}
		}

		if iterateArray {
			items, ok := node.([]any)
			if !ok {
				continue
			}
			selected = append(selected, items...)
			continue
		}

		if index, ok := parseArrayIndex(part); ok {
			items, ok := node.([]any)
			if !ok || index < 0 || index >= len(items) {
				continue
			}
			selected = append(selected, items[index])
			continue
		}

		selected = append(selected, node)
	}
	return selected
}

func lookupJSONObject(object map[string]any, key string) (any, bool) {
	if value, ok := object[key]; ok {
		return value, true
	}
	for actualKey, value := range object {
		if strings.EqualFold(actualKey, key) {
			return value, true
		}
	}
	return nil, false
}

func parseArrayIndex(part string) (int, bool) {
	if !strings.HasPrefix(part, "[") || !strings.HasSuffix(part, "]") {
		return 0, false
	}
	index, err := strconv.Atoi(strings.Trim(part, "[]"))
	return index, err == nil
}

func stringifyJSONValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(data)
	}
}

func sortEventMappings(mappings []EventMapping) {
	sort.SliceStable(mappings, func(i, j int) bool {
		if mappings[i].Priority == mappings[j].Priority {
			return mappings[i].MappingID < mappings[j].MappingID
		}
		return mappings[i].Priority > mappings[j].Priority
	})
}
