package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

type Config struct {
	WebhookSecret string
	ClientTokens  map[string]bool
	CommsTokens   map[string]bool
	ListenAddr    string
}

// maxCommsMessageBytes bounds the message field of a /event/comms request to
// keep a single injection from broadcasting an unbounded payload to all clients.
const maxCommsMessageBytes = 1 << 20 // 1 MiB

func Start(cfg Config) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", webhookHandler(cfg))
	mux.HandleFunc("/v1/webhooks/", providerWebhookHandler(cfg))
	mux.HandleFunc("/event/comms", commsHandler(cfg))
	mux.HandleFunc("/event/", stableEventHandler(cfg))
	mux.HandleFunc("/ws", wsHandler(cfg))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Info().Str("addr", cfg.ListenAddr).Msg("server starting")
	return http.ListenAndServe(cfg.ListenAddr, mux)
}

func webhookHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acceptProviderWebhook(cfg, "github", w, r)
	}
}

func providerWebhookHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/webhooks/"), "/")
		if provider == "" {
			http.Error(w, "provider required", http.StatusBadRequest)
			return
		}
		acceptProviderWebhook(cfg, strings.ToLower(provider), w, r)
	}
}

func acceptProviderWebhook(cfg Config, provider string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	if provider == "github" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !validateHMAC(body, sig, []byte(cfg.WebhookSecret)) {
			log.Warn().Msg("invalid webhook signature")
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	} else if !validateCommsToken(cfg, r.Header.Get("Authorization")) {
		log.Warn().Str("provider", provider).Msg("invalid provider webhook token")
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	event, err := parseProviderWebhook(provider, r.Header, body)
	if err != nil {
		log.Error().Err(err).Msg("failed to parse webhook")
		http.Error(w, "parse error", http.StatusBadRequest)
		return
	}

	log.Info().
		Str("provider", event.Provider).
		Str("event", event.GitHubEvent).
		Str("repo", event.Repo).
		Str("ref", event.Ref).
		Str("delivery", event.DeliveryID).
		Msg("webhook received")

	EventChannel <- *event

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"accepted"}`))
}

// commsHandler accepts free-form agent "comms" events over an authenticated
// HTTP POST and injects them into the shared EventChannel for delivery to
// matching clients, mirroring the GitHub webhook ingress path.
//
// Auth: an Authorization: Bearer <token> header is validated against
// cfg.CommsTokens; if that set is empty it falls back to cfg.ClientTokens.
func commsHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !validateCommsToken(cfg, r.Header.Get("Authorization")) {
			log.Warn().Msg("invalid comms token")
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		var req struct {
			Repo     string `json:"repo"`
			Ref      string `json:"ref"`
			Message  string `json:"message"`
			Sender   string `json:"sender"`
			CloneURL string `json:"clone_url"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "parse error", http.StatusBadRequest)
			return
		}

		if req.Repo == "" || req.Message == "" {
			http.Error(w, "repo and message are required", http.StatusBadRequest)
			return
		}

		// Bound the message so a single comms request can't fan a huge payload
		// out to every subscribed client over WebSocket.
		if len(req.Message) > maxCommsMessageBytes {
			http.Error(w, "message exceeds maximum size", http.StatusRequestEntityTooLarge)
			return
		}

		cloneURL := req.CloneURL
		if cloneURL == "" {
			cloneURL = "https://github.com/" + req.Repo + ".git"
		}

		event := protocol.EventMsg{
			MsgType:       "Event",
			DeliveryID:    fmt.Sprintf("comms-%d", time.Now().UnixNano()),
			GitHubEvent:   "comms",
			Provider:      "custom",
			ExternalEvent: "comms",
			Repo:          req.Repo,
			Ref:           req.Ref,
			Sender:        req.Sender,
			Message:       req.Message,
			CloneURL:      cloneURL,
			Payload:       json.RawMessage(body),
		}

		log.Info().
			Str("event", "comms").
			Str("repo", event.Repo).
			Str("ref", event.Ref).
			Str("delivery", event.DeliveryID).
			Msg("comms event received")

		EventChannel <- event

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"accepted"}`))
	}
}

// stableEventHandler accepts a direct stable internal event on /event/{name}.
// This bypasses provider-specific normalization while preserving the same
// authenticated fan-out path used by comms and generic provider webhooks.
func stableEventHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !validateCommsToken(cfg, r.Header.Get("Authorization")) {
			log.Warn().Msg("invalid stable event token")
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		stableEvent := strings.Trim(strings.TrimPrefix(r.URL.Path, "/event/"), "/")
		if stableEvent == "" || strings.ContainsAny(stableEvent, " \t\r\n/") {
			http.Error(w, "stable event name required", http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		event, err := parseStableEventRequest(stableEvent, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(event.Message) > maxCommsMessageBytes {
			http.Error(w, "message exceeds maximum size", http.StatusRequestEntityTooLarge)
			return
		}

		log.Info().
			Str("stable_event", event.StableEvent).
			Str("repo", event.Repo).
			Str("ref", event.Ref).
			Str("delivery", event.DeliveryID).
			Msg("stable event received")

		EventChannel <- event

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":       "accepted",
			"delivery_id":  event.DeliveryID,
			"stable_event": event.StableEvent,
		})
	}
}

func parseStableEventRequest(stableEvent string, body []byte) (protocol.EventMsg, error) {
	stableEvent = strings.TrimSpace(stableEvent)
	if stableEvent == "" || strings.ContainsAny(stableEvent, " \t\r\n/") {
		return protocol.EventMsg{}, fmt.Errorf("stable event name required")
	}
	var req struct {
		DeliveryID string            `json:"delivery_id"`
		Repo       string            `json:"repo"`
		Ref        string            `json:"ref"`
		Title      string            `json:"title"`
		Body       string            `json:"body"`
		Action     string            `json:"action"`
		Message    string            `json:"message"`
		Sender     string            `json:"sender"`
		CloneURL   string            `json:"clone_url"`
		Metadata   map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return protocol.EventMsg{}, fmt.Errorf("parse error")
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" {
		return protocol.EventMsg{}, fmt.Errorf("repo is required")
	}
	metadata := map[string]string{}
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	if req.Title != "" {
		metadata["title"] = req.Title
	}
	if req.Action != "" {
		metadata["action"] = req.Action
	}
	cloneURL := strings.TrimSpace(req.CloneURL)
	if cloneURL == "" {
		cloneURL = "https://github.com/" + req.Repo + ".git"
	}
	deliveryID := strings.TrimSpace(req.DeliveryID)
	if deliveryID == "" {
		deliveryID = fmt.Sprintf("event-%d", time.Now().UnixNano())
	}
	return protocol.EventMsg{
		MsgType:        "Event",
		DeliveryID:     deliveryID,
		GitHubEvent:    "internal",
		Provider:       "eventic",
		StableEvent:    stableEvent,
		ExternalEvent:  "eventic",
		ExternalAction: strings.TrimSpace(req.Action),
		Repo:           req.Repo,
		Ref:            strings.TrimSpace(req.Ref),
		Action:         strings.TrimSpace(req.Action),
		Sender:         firstNonEmpty(strings.TrimSpace(req.Sender), "eventic-event"),
		Message:        stableEventMessage(req.Message, req.Title, req.Body),
		CloneURL:       cloneURL,
		Metadata:       metadata,
		Payload:        json.RawMessage(body),
	}, nil
}

func stableEventMessage(message, title, body string) string {
	message = strings.TrimSpace(message)
	if message != "" {
		return message
	}
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title != "" && body != "" {
		return title + "\n\n" + body
	}
	return firstNonEmpty(title, body)
}

// validateCommsToken validates an Authorization header value of the form
// "Bearer <token>" against cfg.CommsTokens, falling back to cfg.ClientTokens
// when no dedicated comms tokens are configured.
func validateCommsToken(cfg Config, authHeader string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, prefix))
	if token == "" {
		return false
	}
	if len(cfg.CommsTokens) > 0 {
		return cfg.CommsTokens[token]
	}
	return cfg.ClientTokens[token]
}

func validateHMAC(payload []byte, signature string, secret []byte) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return hmac.Equal(sig, mac.Sum(nil))
}

func parseProviderWebhook(provider string, headers http.Header, body []byte) (*protocol.EventMsg, error) {
	if provider == "github" {
		return parseWebhook(headers.Get("X-GitHub-Event"), headers.Get("X-GitHub-Delivery"), body, safeHeaderMap(headers))
	}
	return parseGenericWebhook(provider, headers, body)
}

func parseWebhook(eventType, deliveryID string, body []byte, headers map[string]string) (*protocol.EventMsg, error) {
	var raw struct {
		Repository struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
		Ref    string `json:"ref"`
		Action string `json:"action"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest *struct {
			Number int `json:"number"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
	}

	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal webhook: %w", err)
	}

	event := &protocol.EventMsg{
		MsgType:        "Event",
		DeliveryID:     deliveryID,
		GitHubEvent:    eventType,
		Provider:       "github",
		ExternalEvent:  eventType,
		ExternalAction: raw.Action,
		Repo:           raw.Repository.FullName,
		Ref:            raw.Ref,
		Action:         raw.Action,
		Sender:         raw.Sender.Login,
		CloneURL:       raw.Repository.CloneURL,
		Headers:        headers,
		Metadata:       webhookMetadata(body, raw.Ref),
		Payload:        body,
	}

	if raw.PullRequest != nil {
		event.PRNumber = raw.PullRequest.Number
		event.Ref = raw.PullRequest.Head.Ref
	}

	return event, nil
}

func parseGenericWebhook(provider string, headers http.Header, body []byte) (*protocol.EventMsg, error) {
	provider = normalizeProviderName(provider)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal webhook: %w", err)
	}

	eventType := providerEventType(provider, headers, payload)
	action := providerAction(provider, eventType, payload)
	repo := providerRepo(provider, payload)
	ref := providerRef(provider, eventType, payload)
	sender := providerSender(provider, payload)
	message := providerMessage(provider, payload)
	cloneURL := providerCloneURL(provider, payload, repo)
	if cloneURL == "" && repo != "" {
		cloneURL = "https://github.com/" + repo + ".git"
	}
	if repo == "" {
		return nil, fmt.Errorf("repo is required or must be derivable")
	}

	deliveryID := firstNonEmpty(
		headers.Get("X-GitHub-Delivery"),
		headers.Get("X-Gitlab-Event-UUID"),
		headers.Get("X-Request-Id"),
		fmt.Sprintf("%s-%d", provider, time.Now().UnixNano()),
	)

	event := &protocol.EventMsg{
		MsgType:        "Event",
		DeliveryID:     deliveryID,
		GitHubEvent:    eventType,
		Provider:       provider,
		ExternalEvent:  eventType,
		ExternalAction: action,
		Repo:           repo,
		Ref:            ref,
		Action:         action,
		Sender:         sender,
		Message:        message,
		CloneURL:       cloneURL,
		Headers:        safeHeaderMap(headers),
		Metadata:       webhookMetadata(body, ref),
		Payload:        body,
	}
	return event, nil
}

func normalizeProviderName(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "alertmanager":
		return "prometheus"
	default:
		return provider
	}
}

func providerEventType(provider string, headers http.Header, payload map[string]any) string {
	switch provider {
	case "gitlab":
		return normalizeProviderEvent(firstNonEmpty(headers.Get("X-Gitlab-Event"), firstPayloadString(payload, "object_kind", "event_name")))
	case "bitbucket":
		return normalizeProviderEvent(firstNonEmpty(headers.Get("X-Event-Key"), firstPayloadString(payload, "event", "event_key", "type")))
	case "prometheus", "alertmanager":
		return "alert"
	case "discord":
		return discordEventType(payload)
	default:
		return normalizeProviderEvent(firstNonEmpty(
			headers.Get("X-GitHub-Event"),
			headers.Get("X-Gitlab-Event"),
			headers.Get("X-Event-Key"),
			firstPayloadString(payload, "event", "event_type", "type", "object_kind"),
			provider,
		))
	}
}

func providerAction(provider, eventType string, payload map[string]any) string {
	switch provider {
	case "gitlab":
		return firstPayloadString(payload,
			"action",
			"object_attributes.action",
			"object_attributes.state",
			"object_attributes.status",
			"build_status",
			"status",
		)
	case "bitbucket":
		return firstPayloadString(payload,
			"commit_status.state",
			"pullrequest.state",
			"push.changes[].new.type",
			"action",
		)
	case "discord":
		return firstPayloadString(payload, "data.name", "event.action", "action", "name", "type")
	case "prometheus":
		return firstPayloadString(payload, "status", "alerts[].status")
	default:
		return firstPayloadString(payload,
			"action",
			"event.action",
			"object_attributes.action",
			"object_attributes.state",
			"status",
			"state",
			"type",
		)
	}
}

func providerRepo(provider string, payload map[string]any) string {
	switch provider {
	case "gitlab":
		return firstPayloadString(payload,
			"repo",
			"project.path_with_namespace",
			"repository.full_name",
			"object_attributes.project.path_with_namespace",
		)
	case "bitbucket":
		return firstPayloadString(payload,
			"repo",
			"repository.full_name",
			"repository.fullName",
			"pullrequest.destination.repository.full_name",
			"commit_status.repository.full_name",
		)
	case "prometheus":
		return firstPayloadString(payload,
			"repo",
			"repository",
			"commonLabels.repo",
			"commonLabels.repository",
			"commonLabels.project",
			"alerts[].labels.repo",
			"alerts[].labels.repository",
			"alerts[].labels.project",
			"labels.repo",
			"labels.repository",
		)
	case "discord":
		return firstPayloadString(payload,
			"repo",
			"metadata.repo",
			"data.repo",
			"data.options[].value",
		)
	default:
		return firstPayloadString(payload,
			"repo",
			"repository.full_name",
			"project.path_with_namespace",
			"metadata.repo",
			"commonLabels.repo",
			"alerts[].labels.repo",
			"labels.repo",
		)
	}
}

func providerRef(provider, eventType string, payload map[string]any) string {
	switch provider {
	case "bitbucket":
		if ref := bitbucketRef(payload); ref != "" {
			return ref
		}
	case "gitlab":
		if ref := firstPayloadString(payload, "ref"); ref != "" {
			return normalizeGitRef("", ref)
		}
		if ref := firstPayloadString(payload, "object_attributes.ref"); ref != "" {
			return normalizeGitRef("branch", ref)
		}
		if branch := firstPayloadString(payload, "object_attributes.target_branch", "object_attributes.source_branch"); branch != "" {
			return normalizeGitRef("branch", branch)
		}
	}
	return normalizeGitRef("", firstPayloadString(payload,
		"ref",
		"push.changes[].new.name",
		"object_attributes.target_branch",
		"commonLabels.ref",
		"alerts[].labels.ref",
		"labels.ref",
	))
}

func providerSender(provider string, payload map[string]any) string {
	switch provider {
	case "gitlab":
		return firstPayloadString(payload, "user_username", "user.name", "user.email", "user_name")
	case "bitbucket":
		return firstPayloadString(payload, "actor.username", "actor.display_name", "actor.nickname")
	case "prometheus":
		return firstPayloadString(payload, "commonLabels.alertname", "alerts[].labels.alertname", "receiver")
	case "discord":
		return firstPayloadString(payload, "member.user.username", "user.username", "author.username", "sender")
	default:
		return firstPayloadString(payload, "sender", "sender.login", "user_username", "user.name", "actor", "commonLabels.alertname")
	}
}

func providerMessage(provider string, payload map[string]any) string {
	switch provider {
	case "gitlab":
		return firstPayloadString(payload, "message", "object_attributes.title", "commit.message", "build_name", "project.name")
	case "bitbucket":
		return firstPayloadString(payload, "message", "pullrequest.title", "commit_status.description", "push.changes[].new.target.message")
	case "discord":
		return firstPayloadString(payload, "message", "content", "data.name", "event.body", "title", "body")
	case "prometheus":
		return firstPayloadString(payload, "commonAnnotations.summary", "commonAnnotations.description", "alerts[].annotations.summary", "alerts[].annotations.description")
	default:
		return firstPayloadString(payload, "message", "title", "body", "commonAnnotations.summary", "commonAnnotations.description")
	}
}

func providerCloneURL(provider string, payload map[string]any, repo string) string {
	switch provider {
	case "gitlab":
		return firstPayloadString(payload, "project.git_http_url", "project.http_url", "repository.git_http_url", "clone_url")
	case "bitbucket":
		return firstPayloadString(payload, "repository.links.html.href", "pullrequest.destination.repository.links.html.href", "clone_url")
	default:
		return firstPayloadString(payload, "clone_url", "repository.clone_url", "project.git_http_url", "project.http_url")
	}
}

func bitbucketRef(payload map[string]any) string {
	refType := firstPayloadString(payload, "push.changes[].new.type", "push.changes[].old.type")
	refName := firstPayloadString(payload, "push.changes[].new.name", "push.changes[].old.name")
	if refName != "" {
		return normalizeGitRef(refType, refName)
	}
	if branch := firstPayloadString(payload, "pullrequest.destination.branch.name", "pullrequest.source.branch.name"); branch != "" {
		return normalizeGitRef("branch", branch)
	}
	return ""
}

func normalizeGitRef(refType, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "refs/") {
		return value
	}
	switch strings.ToLower(strings.TrimSpace(refType)) {
	case "tag":
		return "refs/tags/" + value
	case "branch", "named_branch":
		return "refs/heads/" + value
	default:
		return value
	}
}

func discordEventType(payload map[string]any) string {
	raw := firstPayloadString(payload, "event", "event.type", "event_type")
	if raw != "" {
		return normalizeProviderEvent(raw)
	}
	switch firstPayloadString(payload, "type") {
	case "1":
		return "ping"
	case "2":
		return "application_command"
	case "3":
		return "message_component"
	case "4":
		return "application_command_autocomplete"
	case "5":
		return "modal_submit"
	default:
		return "message"
	}
}

func normalizeProviderEvent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, " hook")
	value = strings.ReplaceAll(value, " ", "_")
	value = strings.ReplaceAll(value, ":", ".")
	return value
}

func safeHeaderMap(headers http.Header) map[string]string {
	out := map[string]string{}
	for name, values := range headers {
		switch strings.ToLower(name) {
		case "authorization", "cookie", "x-hub-signature", "x-hub-signature-256":
			continue
		}
		if len(values) > 0 {
			out[name] = values[0]
		}
	}
	return out
}

func webhookMetadata(body []byte, ref string) map[string]string {
	metadata := map[string]string{}
	if ref != "" {
		metadata["ref"] = ref
		if strings.HasPrefix(ref, "refs/tags/") {
			metadata["target_environment"] = "production"
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return metadata
	}
	if sha := firstPayloadString(payload, "after", "head_commit.id", "workflow_run.head_sha"); sha != "" {
		metadata["commit_sha"] = sha
	}
	if sha := firstPayloadString(payload,
		"checkout_sha",
		"object_attributes.sha",
		"object_attributes.last_commit.id",
		"push.changes[].new.target.hash",
		"commit_status.commit.hash",
		"commit.hash",
	); sha != "" && metadata["commit_sha"] == "" {
		metadata["commit_sha"] = sha
	}
	if severity := firstPayloadString(payload,
		"alert.security_advisory.severity",
		"security_advisory.severity",
		"commonLabels.severity",
		"alerts[].labels.severity",
		"vulnerabilities[].severity",
		"vulnerability.severity",
		"finding.severity",
		"alert.rule.severity",
		"severity",
	); severity != "" {
		metadata["severity"] = strings.ToUpper(severity)
	}
	for _, key := range []string{"alertname", "cluster", "namespace", "service", "environment"} {
		if value := firstPayloadString(payload, "commonLabels."+key, "alerts[].labels."+key, "labels."+key); value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

func firstPayloadString(payload map[string]any, paths ...string) string {
	for _, path := range paths {
		for _, value := range payloadPathValues([]any{payload}, path) {
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func payloadPathValues(nodes []any, path string) []string {
	for _, part := range strings.Split(path, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil
		}
		nodes = selectPayloadPart(nodes, part)
		if len(nodes) == 0 {
			return nil
		}
	}
	values := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if value := payloadValueString(node); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func selectPayloadPart(nodes []any, part string) []any {
	iterateArray := strings.HasSuffix(part, "[]")
	if iterateArray {
		part = strings.TrimSuffix(part, "[]")
	}
	var selected []any
	for _, node := range nodes {
		object, ok := node.(map[string]any)
		if !ok {
			continue
		}
		value, ok := lookupPayloadObject(object, part)
		if !ok {
			continue
		}
		if iterateArray {
			items, ok := value.([]any)
			if !ok {
				continue
			}
			selected = append(selected, items...)
			continue
		}
		selected = append(selected, value)
	}
	return selected
}

func lookupPayloadObject(object map[string]any, key string) (any, bool) {
	if value, ok := object[key]; ok {
		return value, true
	}
	for actual, value := range object {
		if strings.EqualFold(actual, key) {
			return value, true
		}
	}
	return nil, false
}

func payloadValueString(value any) string {
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
		return fmt.Sprintf("%v", typed)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(data)
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

func wsHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			log.Error().Err(err).Msg("websocket accept failed")
			return
		}
		defer func() {
			RemoveClient(conn)
			conn.Close(websocket.StatusNormalClosure, "closing")
		}()

		_, authData, err := conn.Read(r.Context())
		if err != nil {
			log.Error().Err(err).Msg("failed to read auth message")
			return
		}

		var auth protocol.AuthMsg
		if err := json.Unmarshal(authData, &auth); err != nil || auth.MsgType != "Auth" {
			resp, _ := json.Marshal(protocol.AuthResult{MsgType: "AuthFail", Reason: "invalid auth message"})
			conn.Write(r.Context(), websocket.MessageText, resp)
			return
		}

		if !cfg.ClientTokens[auth.Token] {
			resp, _ := json.Marshal(protocol.AuthResult{MsgType: "AuthFail", Reason: "invalid token"})
			conn.Write(r.Context(), websocket.MessageText, resp)
			return
		}

		RegisterClient(conn, auth.ClientID)
		resp, _ := json.Marshal(protocol.AuthResult{MsgType: "AuthOK"})
		conn.Write(r.Context(), websocket.MessageText, resp)

		for {
			_, msg, err := conn.Read(r.Context())
			if err != nil {
				log.Debug().Err(err).Str("client", auth.ClientID).Msg("client read error")
				return
			}

			var env protocol.Envelope
			if err := json.Unmarshal(msg, &env); err != nil {
				log.Error().Err(err).Msg("invalid message")
				continue
			}

			switch env.MsgType {
			case "Subscribe":
				var sub protocol.SubscribeMsg
				if err := json.Unmarshal(msg, &sub); err == nil {
					SetSubscription(conn, sub.Patterns)
				}
			case "Status":
				var status protocol.StatusMsg
				if err := json.Unmarshal(msg, &status); err == nil {
					log.Info().
						Str("delivery", status.DeliveryID).
						Str("state", status.State).
						Str("desc", status.Description).
						Msg("client status report")
				}
			case "Pong":
				// heartbeat acknowledged
			default:
				log.Warn().Str("type", env.MsgType).Msg("unknown message type from client")
			}
		}
	}
}
