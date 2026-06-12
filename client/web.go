package client

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

const (
	defaultWebListen    = "127.0.0.1:16384"
	defaultWebMaxEvents = 100
	defaultWebMaxOutput = 65536
)

// WebConfig controls the client-local execution console.
type WebConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Listen         string `yaml:"listen"`
	MaxEvents      int    `yaml:"max_events"`
	MaxOutputBytes int    `yaml:"max_output_bytes"`
}

type ReplayDispatcher func(context.Context, protocol.EventMsg)

type ReplayRef struct {
	Ref  string `json:"ref"`
	Type string `json:"type"`
	Hash string `json:"hash,omitempty"`
}

type replayRequest struct {
	Repo     string `json:"repo"`
	EventKey string `json:"event_key"`
	Ref      string `json:"ref"`
}

// ExecutionEvent is a compact, local-only view of an Eventic event execution.
type ExecutionEvent struct {
	ID             string          `json:"id"`
	DeliveryID     string          `json:"delivery_id"`
	Repo           string          `json:"repo"`
	Ref            string          `json:"ref,omitempty"`
	Event          string          `json:"event"`
	Action         string          `json:"action,omitempty"`
	Sender         string          `json:"sender,omitempty"`
	State          string          `json:"state"`
	Description    string          `json:"description,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
	DurationMillis int64           `json:"duration_ms,omitempty"`
	Hooks          []HookExecution `json:"hooks"`
}

// HookExecution records one local hook run and its bounded combined output.
type HookExecution struct {
	Name           string     `json:"name"`
	State          string     `json:"state"`
	Output         string     `json:"output,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	DurationMillis int64      `json:"duration_ms,omitempty"`
}

type ExecutionLog struct {
	mu             sync.RWMutex
	events         []ExecutionEvent
	byDeliveryID   map[string]int
	maxEvents      int
	maxOutputBytes int
	subscribers    map[chan ExecutionEvent]struct{}
}

func NewExecutionLog(cfg WebConfig) *ExecutionLog {
	maxEvents := cfg.MaxEvents
	if maxEvents <= 0 {
		maxEvents = defaultWebMaxEvents
	}
	maxOutputBytes := cfg.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultWebMaxOutput
	}

	return &ExecutionLog{
		byDeliveryID:   make(map[string]int),
		maxEvents:      maxEvents,
		maxOutputBytes: maxOutputBytes,
		subscribers:    make(map[chan ExecutionEvent]struct{}),
	}
}

func (l *ExecutionLog) StartEvent(event protocol.EventMsg) ExecutionEvent {
	now := time.Now()
	rec := ExecutionEvent{
		ID:         event.DeliveryID,
		DeliveryID: event.DeliveryID,
		Repo:       event.Repo,
		Ref:        event.Ref,
		Event:      event.GitHubEvent,
		Action:     event.Action,
		Sender:     event.Sender,
		State:      "running",
		StartedAt:  now,
		UpdatedAt:  now,
	}

	l.mu.Lock()
	l.events = append([]ExecutionEvent{rec}, l.events...)
	l.reindexLocked()
	l.trimLocked()
	snapshot := cloneEventLocked(rec)
	l.mu.Unlock()

	l.publish(snapshot)
	return snapshot
}

func (l *ExecutionLog) FinishEvent(deliveryID, state, desc string) *ExecutionEvent {
	return l.updateEvent(deliveryID, func(rec *ExecutionEvent) {
		now := time.Now()
		rec.State = state
		rec.Description = trimString(desc, l.maxOutputBytes)
		rec.FinishedAt = &now
		rec.UpdatedAt = now
		rec.DurationMillis = now.Sub(rec.StartedAt).Milliseconds()
	})
}

func (l *ExecutionLog) AddHook(deliveryID, name, state, output string) {
	now := time.Now()
	l.updateEvent(deliveryID, func(rec *ExecutionEvent) {
		finishedAt := now
		rec.Hooks = append(rec.Hooks, HookExecution{
			Name:       name,
			State:      state,
			Output:     trimString(output, l.maxOutputBytes),
			StartedAt:  now,
			FinishedAt: &finishedAt,
		})
		rec.UpdatedAt = now
	})
}

func (l *ExecutionLog) StartHook(deliveryID, name string) {
	now := time.Now()
	l.updateEvent(deliveryID, func(rec *ExecutionEvent) {
		rec.Hooks = append(rec.Hooks, HookExecution{
			Name:      name,
			State:     "running",
			StartedAt: now,
		})
		rec.UpdatedAt = now
	})
}

func (l *ExecutionLog) FinishHook(deliveryID, name, state, output string) {
	l.updateEvent(deliveryID, func(rec *ExecutionEvent) {
		now := time.Now()
		for i := len(rec.Hooks) - 1; i >= 0; i-- {
			if rec.Hooks[i].Name == name && rec.Hooks[i].FinishedAt == nil {
				rec.Hooks[i].State = state
				rec.Hooks[i].Output = trimString(output, l.maxOutputBytes)
				rec.Hooks[i].FinishedAt = &now
				rec.Hooks[i].DurationMillis = now.Sub(rec.Hooks[i].StartedAt).Milliseconds()
				rec.UpdatedAt = now
				return
			}
		}
		finishedAt := now
		rec.Hooks = append(rec.Hooks, HookExecution{
			Name:       name,
			State:      state,
			Output:     trimString(output, l.maxOutputBytes),
			StartedAt:  now,
			FinishedAt: &finishedAt,
		})
		rec.UpdatedAt = now
	})
}

func (l *ExecutionLog) Events() []ExecutionEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]ExecutionEvent, 0, len(l.events))
	for _, event := range l.events {
		out = append(out, cloneEventLocked(event))
	}
	return out
}

func (l *ExecutionLog) Subscribe() (<-chan ExecutionEvent, func()) {
	ch := make(chan ExecutionEvent, 16)
	l.mu.Lock()
	l.subscribers[ch] = struct{}{}
	l.mu.Unlock()

	cancel := func() {
		l.mu.Lock()
		if _, ok := l.subscribers[ch]; ok {
			delete(l.subscribers, ch)
			close(ch)
		}
		l.mu.Unlock()
	}
	return ch, cancel
}

func (l *ExecutionLog) updateEvent(deliveryID string, mutate func(*ExecutionEvent)) *ExecutionEvent {
	l.mu.Lock()
	idx, ok := l.byDeliveryID[deliveryID]
	if !ok {
		l.mu.Unlock()
		return nil
	}
	mutate(&l.events[idx])
	snapshot := cloneEventLocked(l.events[idx])
	l.mu.Unlock()

	l.publish(snapshot)
	return &snapshot
}

func (l *ExecutionLog) publish(event ExecutionEvent) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	for ch := range l.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (l *ExecutionLog) trimLocked() {
	if len(l.events) <= l.maxEvents {
		return
	}
	l.events = l.events[:l.maxEvents]
	l.reindexLocked()
}

func (l *ExecutionLog) reindexLocked() {
	clear(l.byDeliveryID)
	for i, event := range l.events {
		l.byDeliveryID[event.DeliveryID] = i
	}
}

func StartWebConsole(ctx context.Context, cfg Config, logStore *ExecutionLog, projectStore *ProjectStore, replay ReplayDispatcher) error {
	listen := cfg.Web.Listen
	if listen == "" {
		listen = defaultWebListen
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", webIndexHandler)
	mux.HandleFunc("/events", eventsHandler(logStore))
	mux.HandleFunc("/events/stream", eventsStreamHandler(logStore))
	mux.HandleFunc("/projects", projectsHandler(projectStore))
	mux.HandleFunc("/projects/", projectsHandler(projectStore))
	mux.HandleFunc("/replay", replayHandler(projectStore, replay))
	mux.HandleFunc("/replay/refs", replayRefsHandler(cfg))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("web console shutdown failed")
		}
	}()

	log.Info().Str("listen", listen).Msg("client web console starting")
	err := srv.ListenAndServe()
	if err == nil || err == http.ErrServerClosed {
		return nil
	}
	return err
}

func projectsHandler(projectStore *ProjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		repo := strings.TrimPrefix(r.URL.Path, "/projects/")
		if repo != r.URL.Path {
			repo = strings.Trim(repo, "/")
			if repo == "" || strings.Count(repo, "/") != 1 {
				http.NotFound(w, r)
				return
			}
			project, err := projectStore.GetProject(r.Context(), repo)
			if isNotFound(err) {
				http.NotFound(w, r)
				return
			}
			if err != nil {
				http.Error(w, "failed to read project", http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(project)
			return
		}

		if r.URL.Path != "/projects" {
			http.NotFound(w, r)
			return
		}

		projects, err := projectStore.ListProjects(r.Context())
		if err != nil {
			http.Error(w, "failed to read projects", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(projects)
	}
}

func replayRefsHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		repo := strings.TrimSpace(r.URL.Query().Get("repo"))
		repoPath, err := repoPathForWeb(cfg.ReposDir, repo)
		if err != nil {
			http.Error(w, "invalid repo", http.StatusBadRequest)
			return
		}
		refs, err := listReplayRefs(r.Context(), repoPath)
		if err != nil {
			http.Error(w, "failed to list refs", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(refs)
	}
}

func replayHandler(projectStore *ProjectStore, replay ReplayDispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if replay == nil {
			http.Error(w, "replay unavailable", http.StatusServiceUnavailable)
			return
		}

		var req replayRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid replay request", http.StatusBadRequest)
			return
		}
		req.Repo = strings.TrimSpace(req.Repo)
		req.Ref = strings.TrimSpace(req.Ref)
		req.EventKey = strings.TrimSpace(req.EventKey)
		if req.Repo == "" || req.Ref == "" {
			http.Error(w, "repo and ref are required", http.StatusBadRequest)
			return
		}

		if _, err := repoPathForWeb("", req.Repo); err != nil {
			http.Error(w, "invalid repo", http.StatusBadRequest)
			return
		}
		project, err := projectStore.GetProject(r.Context(), req.Repo)
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "failed to read project", http.StatusInternalServerError)
			return
		}
		if req.EventKey == "" {
			req.EventKey = defaultReplayEventKey(project)
		}
		if !projectHasReplayEvent(project, req.EventKey) {
			http.Error(w, "event is not configured for project", http.StatusBadRequest)
			return
		}

		eventName, action := replayEventFromKey(req.EventKey, project.Event, project.Action)
		deliveryID := fmt.Sprintf("manual-%d", time.Now().UnixNano())
		payload := replayPayload(req.Repo, req.Ref, eventName, action, deliveryID)
		event := protocol.EventMsg{
			MsgType:     "Event",
			DeliveryID:  deliveryID,
			GitHubEvent: eventName,
			Repo:        req.Repo,
			Ref:         req.Ref,
			Action:      action,
			Sender:      "eventic-web",
			CloneURL:    "https://github.com/" + req.Repo + ".git",
			Payload:     payload,
		}

		replay(r.Context(), event)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"delivery_id": deliveryID,
			"status":      "accepted",
		})
	}
}

func repoPathForWeb(reposDir, repo string) (string, error) {
	if repo == "" || strings.Count(repo, "/") != 1 || strings.Contains(repo, "..") {
		return "", fmt.Errorf("invalid repo")
	}
	if reposDir == "" {
		return "", nil
	}
	root := filepath.Clean(reposDir)
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(repo)))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", fmt.Errorf("repo outside root")
	}
	if info, err := os.Stat(filepath.Join(path, ".git")); err != nil || !info.IsDir() {
		return "", fmt.Errorf("repo checkout not found")
	}
	return path, nil
}

func listReplayRefs(ctx context.Context, repoPath string) ([]ReplayRef, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "for-each-ref", "--format=%(refname)\t%(objectname)", "refs/heads", "refs/tags")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	refs := []ReplayRef{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		ref := parts[0]
		hash := ""
		if len(parts) == 2 {
			hash = parts[1]
		}
		refType := "ref"
		switch {
		case strings.HasPrefix(ref, "refs/heads/"):
			refType = "branch"
		case strings.HasPrefix(ref, "refs/tags/"):
			refType = "tag"
		}
		refs = append(refs, ReplayRef{Ref: ref, Type: refType, Hash: hash})
	}
	return refs, nil
}

func defaultReplayEventKey(project *ProjectState) string {
	if project == nil {
		return "push"
	}
	for _, event := range project.ConfiguredEvents {
		raw := replayRawEventKey(event.EventKey)
		if raw != "" && raw != "global" {
			return raw
		}
	}
	if project.Event != "" {
		if project.Action != "" {
			return project.Event + "." + project.Action
		}
		return project.Event
	}
	return "push"
}

func projectHasReplayEvent(project *ProjectState, eventKey string) bool {
	if project == nil {
		return false
	}
	if len(project.ConfiguredEvents) == 0 {
		return eventKey == defaultReplayEventKey(project)
	}
	raw := replayRawEventKey(eventKey)
	hasGlobal := false
	for _, configured := range project.ConfiguredEvents {
		configuredRaw := replayRawEventKey(configured.EventKey)
		if configuredRaw == raw {
			return true
		}
		hasGlobal = hasGlobal || configuredRaw == "global"
	}
	return hasGlobal && raw != ""
}

func replayRawEventKey(eventKey string) string {
	raw := strings.SplitN(strings.TrimSpace(eventKey), ":", 2)[0]
	return raw
}

func replayEventFromKey(eventKey, fallbackEvent, fallbackAction string) (string, string) {
	raw := replayRawEventKey(eventKey)
	if raw == "" || raw == "global" {
		if fallbackEvent != "" {
			return fallbackEvent, fallbackAction
		}
		return "push", ""
	}
	return splitConfiguredEventKey(raw)
}

func replayPayload(repo, ref, eventName, action, deliveryID string) json.RawMessage {
	payload := map[string]any{
		"ref": ref,
		"repository": map[string]any{
			"full_name": repo,
			"clone_url": "https://github.com/" + repo + ".git",
		},
		"sender": map[string]any{
			"login": "eventic-web",
		},
		"eventic_replay": true,
		"delivery_id":    deliveryID,
	}
	if action != "" {
		payload["action"] = action
	}
	if eventName == "push" {
		payload["after"] = ""
	}
	data, _ := json.Marshal(payload)
	return data
}

func eventsHandler(logStore *ExecutionLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logStore.Events())
	}
}

func eventsStreamHandler(logStore *ExecutionLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch, cancel := logStore.Subscribe()
		defer cancel()

		for _, event := range logStore.Events() {
			if err := writeSSE(w, event); err != nil {
				return
			}
		}
		flusher.Flush()

		heartbeat := time.NewTicker(30 * time.Second)
		defer heartbeat.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-heartbeat.C:
				fmt.Fprint(w, ": heartbeat\n\n")
				flusher.Flush()
			case event := <-ch:
				if err := writeSSE(w, event); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, event ExecutionEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: event\nid: %s\ndata: %s\n\n", event.DeliveryID, data)
	return err
}

func webIndexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	webIndexTemplate.Execute(w, nil)
}

func cloneEventLocked(event ExecutionEvent) ExecutionEvent {
	event.Hooks = append([]HookExecution(nil), event.Hooks...)
	return event
}

func trimString(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	if maxBytes <= len("... [truncated]") {
		return value[:maxBytes]
	}
	return value[:maxBytes-len("... [truncated]")] + "... [truncated]"
}

var webIndexTemplate = template.Must(template.New("index").Parse(strings.TrimSpace(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Eventic Client</title>
  <style>
    :root {
      color-scheme: light dark;
      --bg: #f6f7f9;
      --panel: #ffffff;
      --text: #17202a;
      --muted: #617084;
      --border: #d8dee8;
      --accent: #0f766e;
      --success: #15803d;
      --failure: #b91c1c;
      --running: #b45309;
      --code: #101418;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #111418;
        --panel: #191e24;
        --text: #ecf1f7;
        --muted: #a1adbb;
        --border: #323a45;
        --code: #080a0d;
      }
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: var(--bg);
      color: var(--text);
      height: 100vh;
      display: flex;
      flex-direction: column;
    }
    header {
      height: 56px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0 20px;
      border-bottom: 1px solid var(--border);
      background: var(--panel);
    }
    h1 {
      margin: 0;
      font-size: 18px;
      font-weight: 650;
    }
    .status {
      color: var(--muted);
      font-size: 13px;
    }
    main {
      display: grid;
      grid-template-columns: minmax(320px, 440px) minmax(0, 1fr);
      flex: 1 1 auto;
      min-height: 0;
    }
    .sidebar {
      border-right: 1px solid var(--border);
      background: var(--panel);
      overflow: auto;
      max-height: none;
      padding: 12px;
      display: flex;
      flex-direction: column;
      gap: 12px;
    }
    .well {
      border: 1px solid var(--border);
      border-radius: 8px;
      overflow: hidden;
      background: var(--panel);
    }
    .well-title {
      padding: 10px 12px;
      border-bottom: 1px solid var(--border);
      color: var(--muted);
      font-size: 12px;
      font-weight: 700;
      letter-spacing: 0;
      text-transform: uppercase;
    }
    .active-well {
      flex: 0 0 auto;
    }
    .existing-well {
      flex: 1 1 auto;
      min-height: 0;
      display: flex;
      flex-direction: column;
    }
    .search-box {
      padding: 10px 12px;
      border-bottom: 1px solid var(--border);
    }
    .search-input {
      width: 100%;
      height: 34px;
      border: 1px solid var(--border);
      border-radius: 6px;
      padding: 0 10px;
      background: var(--bg);
      color: var(--text);
      font: inherit;
      font-size: 13px;
    }
    .search-input:focus {
      outline: 2px solid color-mix(in srgb, var(--accent) 45%, transparent);
      outline-offset: 1px;
    }
    .project-list {
      overflow: auto;
    }
    .existing-well .project-list {
      flex: 1 1 auto;
      min-height: 160px;
    }
    .event {
      width: 100%;
      min-height: 84px;
      padding: 14px 16px;
      border: 0;
      border-bottom: 1px solid var(--border);
      background: transparent;
      color: inherit;
      text-align: left;
      cursor: pointer;
    }
    .event:hover, .event[aria-selected="true"] {
      background: color-mix(in srgb, var(--accent) 11%, transparent);
    }
    .well .event:last-child {
      border-bottom: 0;
    }
    .event-head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 6px;
    }
    .repo {
      min-width: 0;
      overflow-wrap: anywhere;
      font-weight: 650;
      font-size: 14px;
    }
    .pill {
      flex: 0 0 auto;
      border: 1px solid currentColor;
      border-radius: 999px;
      padding: 2px 8px;
      font-size: 12px;
      line-height: 18px;
      text-transform: uppercase;
    }
    .success { color: var(--success); }
    .failure { color: var(--failure); }
    .running { color: var(--running); }
    .skipped { color: var(--muted); }
    .meta {
      color: var(--muted);
      font-size: 13px;
      line-height: 1.45;
      overflow-wrap: anywhere;
    }
    .detail {
      min-width: 0;
      padding: 18px 20px;
      overflow: auto;
      max-height: none;
    }
    .detail h2 {
      margin: 0 0 8px;
      font-size: 20px;
      overflow-wrap: anywhere;
    }
    .detail-head {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 8px;
    }
    .detail-head h2 {
      margin-bottom: 0;
    }
    .toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 16px;
      color: var(--muted);
      font-size: 13px;
    }
    .replay-controls {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      flex-wrap: wrap;
      gap: 8px;
    }
    .replay-controls select,
    .replay-controls button {
      height: 34px;
      border: 1px solid var(--border);
      border-radius: 6px;
      background: var(--panel);
      color: var(--text);
      font: inherit;
      font-size: 13px;
    }
    .replay-controls select {
      max-width: 240px;
      padding: 0 8px;
    }
    .replay-controls button {
      padding: 0 12px;
      cursor: pointer;
      background: var(--accent);
      border-color: var(--accent);
      color: #fff;
      font-weight: 650;
    }
    .replay-controls button:disabled {
      cursor: not-allowed;
      opacity: .55;
    }
    .replay-status {
      flex: 1 0 100%;
      color: var(--muted);
      font-size: 12px;
      text-align: right;
    }
    .hook {
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--panel);
      margin: 12px 0;
      overflow: hidden;
    }
    .history-event {
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--panel);
      margin: 12px 0 16px;
      overflow: hidden;
    }
    .history-title {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      padding: 12px;
      border-bottom: 1px solid var(--border);
    }
    .history-body {
      padding: 0 12px 12px;
    }
    .hook-title {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      padding: 10px 12px;
      border-bottom: 1px solid var(--border);
      font-size: 13px;
      font-weight: 650;
    }
    pre {
      margin: 0;
      min-height: 72px;
      padding: 12px;
      overflow: auto;
      background: var(--code);
      color: #e8eef7;
      font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }
    .empty {
      color: var(--muted);
      padding: 24px;
    }
    .event-footer {
      flex: 0 0 190px;
      border-top: 1px solid var(--border);
      background: var(--panel);
      min-height: 0;
      display: flex;
      flex-direction: column;
    }
    .footer-title {
      padding: 10px 20px;
      border-bottom: 1px solid var(--border);
      color: var(--muted);
      font-size: 12px;
      font-weight: 700;
      text-transform: uppercase;
    }
    .event-queue {
      overflow: auto;
      padding: 8px 20px 14px;
    }
    .queue-row {
      display: grid;
      grid-template-columns: minmax(180px, 1.2fr) minmax(110px, .7fr) minmax(100px, .55fr) minmax(170px, .9fr);
      gap: 12px;
      align-items: center;
      padding: 8px 0;
      border-bottom: 1px solid var(--border);
      font-size: 13px;
    }
    .queue-row:last-child {
      border-bottom: 0;
    }
    @media (max-width: 760px) {
      main { grid-template-columns: 1fr; }
      .sidebar {
        max-height: 45vh;
        border-right: 0;
        border-bottom: 1px solid var(--border);
      }
      .detail-head { flex-direction: column; }
      .replay-controls { justify-content: flex-start; }
      .replay-status { text-align: left; }
      .detail { max-height: none; }
      .event-footer { flex-basis: 220px; }
      .queue-row { grid-template-columns: 1fr; gap: 4px; }
    }
  </style>
</head>
<body>
  <header>
    <h1>Eventic Client</h1>
    <div class="status" id="connection">connecting</div>
  </header>
  <main>
    <section class="sidebar">
      <section class="well active-well">
        <div class="well-title">Active Projects</div>
        <div class="project-list" id="active-projects">
          <div class="empty">No active projects.</div>
        </div>
      </section>
      <section class="well existing-well">
        <div class="well-title">Existing Projects</div>
        <div class="search-box">
          <input class="search-input" id="project-search" type="search" placeholder="Search repositories" autocomplete="off">
        </div>
        <div class="project-list" id="existing-projects">
          <div class="empty">No projects yet.</div>
        </div>
      </section>
    </section>
    <section class="detail" id="detail">
      <div class="empty">Select a project to inspect configured outputs.</div>
    </section>
  </main>
  <footer class="event-footer">
    <div class="footer-title">Event Queue</div>
    <div class="event-queue" id="event-queue">
      <div class="empty">No events received yet.</div>
    </div>
  </footer>
  <script>
    const existingProjects = new Map();
    const eventQueue = new Map();
    const replayRefs = new Map();
    const replayRefLoads = new Map();
    const eventQueueLimit = 100;
    let selectedRepo = "";
    let selectedProjectDetail = null;
    let selectedReplayEvent = "";
    let selectedReplayRef = "";
    let replayStatus = "";
    let projectRefreshTimer = null;
    let projectSearch = "";
    let lastOpenRefresh = 0;
    const activeEl = document.getElementById("active-projects");
    const existingEl = document.getElementById("existing-projects");
    const eventQueueEl = document.getElementById("event-queue");
    const searchEl = document.getElementById("project-search");
    const detailEl = document.getElementById("detail");
    const connectionEl = document.getElementById("connection");

    function stateClass(state) {
      return ["success", "failure", "running", "skipped"].includes(state) ? state : "";
    }

    function fmtDate(value) {
      if (!value) return "";
      return new Date(value).toLocaleString();
    }

    function escapeHTML(value) {
      return String(value ?? "").replace(/[&<>"']/g, ch => ({
        "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
      }[ch]));
    }

    function eventTime(event) {
      return new Date(event.updated_at || event.started_at || 0).getTime();
    }

    function upsertQueueEvent(event) {
      if (!event || !event.delivery_id) return;
      eventQueue.set(event.delivery_id, event);
      const sorted = sortedQueueEvents();
      while (sorted.length > eventQueueLimit) {
        eventQueue.delete(sorted.pop().delivery_id);
      }
      renderEventQueue();
      scheduleProjectRefresh();
    }

    function sortedExistingProjects() {
      const query = projectSearch.trim().toLowerCase();
      return [...existingProjects.values()].filter(project =>
        !query || project.repo.toLowerCase().includes(query)
      ).sort((a, b) =>
        new Date(b.updated_at || b.started_at || 0).getTime() -
        new Date(a.updated_at || a.started_at || 0).getTime() ||
        a.repo.localeCompare(b.repo)
      );
    }

    function sortedQueueEvents() {
      return [...eventQueue.values()].sort((a, b) => eventTime(b) - eventTime(a));
    }

    function configuredEvents(project) {
      return project && Array.isArray(project.configured_events) ? project.configured_events : [];
    }

    function latestConfiguredEvent(project) {
      const events = configuredEvents(project).filter(event => event.state && event.state !== "no_runs");
      return events.sort((a, b) => eventTime(b) - eventTime(a))[0] || configuredEvents(project)[0] || null;
    }

    function sortedActiveConfiguredEvents() {
      const rows = [];
      existingProjects.forEach(project => {
        configuredEvents(project).forEach(event => {
          if (event.state === "running") rows.push({ project, event });
        });
      });
      return rows.sort((a, b) => eventTime(b.event) - eventTime(a.event));
    }

    function eventLabel(event) {
      return escapeHTML(event.event || "event") + (event.action ? "." + escapeHTML(event.action) : "");
    }

    function replayRawEventKey(eventKey) {
      return String(eventKey || "").split(":")[0];
    }

    function replayEventOptions(project) {
      const options = new Map();
      let hasGlobal = false;
      configuredEvents(project).forEach(event => {
        const raw = replayRawEventKey(event.event_key);
        if (!raw) return;
        if (raw === "global") {
          hasGlobal = true;
          return;
        }
        options.set(raw, { value: raw, label: raw });
      });
      if (hasGlobal) {
        if (!options.has("push")) {
          options.set("push", { value: "push", label: "push (global)" });
        }
        const fallback = (project.event || "push") + (project.action ? "." + project.action : "");
        if (fallback !== "push" && !options.has(fallback)) {
          options.set(fallback, { value: fallback, label: fallback + " (global)" });
        }
      }
      if (!options.size) {
        const fallback = (project.event || "push") + (project.action ? "." + project.action : "");
        options.set(fallback, { value: fallback, label: fallback });
      }
      return [...options.values()];
    }

    function fallbackRef(project) {
      if (!project || !project.ref || project.ref === "HEAD") return "";
      if (project.ref.startsWith("refs/")) return project.ref;
      return "refs/heads/" + project.ref;
    }

    function replayRefOptions(project) {
      const refs = replayRefs.get(project.repo) || [];
      if (refs.length) return refs;
      const ref = fallbackRef(project);
      return ref ? [{ ref: ref, type: "current" }] : [];
    }

    function ensureReplayRefs(repo) {
      if (!repo || replayRefs.has(repo)) return Promise.resolve();
      if (replayRefLoads.has(repo)) return replayRefLoads.get(repo);
      const load = fetch("/replay/refs?repo=" + encodeURIComponent(repo)).then(resp => {
        if (!resp.ok) throw new Error("refs unavailable");
        return resp.json();
      }).then(refs => {
        replayRefs.set(repo, Array.isArray(refs) ? refs : []);
      }).catch(() => {
        replayRefs.set(repo, []);
      }).finally(() => {
        replayRefLoads.delete(repo);
        if (selectedRepo === repo) renderProjectDetail();
      });
      replayRefLoads.set(repo, load);
      return load;
    }

    function renderReplayControls(project) {
      ensureReplayRefs(project.repo);
      const eventOptions = replayEventOptions(project);
      if (!eventOptions.some(option => option.value === selectedReplayEvent)) {
        selectedReplayEvent = eventOptions[0] ? eventOptions[0].value : "";
      }
      const refOptions = replayRefOptions(project);
      if (!refOptions.some(option => option.ref === selectedReplayRef)) {
        selectedReplayRef = refOptions[0] ? refOptions[0].ref : "";
      }
      const refsLoading = replayRefLoads.has(project.repo);
      const disabled = !selectedReplayEvent || !selectedReplayRef || replayStatus === "triggering";
      return '<div class="replay-controls">' +
        '<select id="replay-event" aria-label="Event">' +
          eventOptions.map(option =>
            '<option value="' + escapeHTML(option.value) + '"' + (option.value === selectedReplayEvent ? " selected" : "") + '>' + escapeHTML(option.label) + '</option>'
          ).join("") +
        '</select>' +
        '<select id="replay-ref" aria-label="Ref">' +
          (refOptions.length ? refOptions.map(option =>
            '<option value="' + escapeHTML(option.ref) + '"' + (option.ref === selectedReplayRef ? " selected" : "") + '>' + escapeHTML(option.ref) + '</option>'
          ).join("") : '<option value="">' + (refsLoading ? "Loading refs" : "No refs") + '</option>') +
        '</select>' +
        '<button id="replay-trigger" type="button"' + (disabled ? " disabled" : "") + '>Trigger</button>' +
        (replayStatus && replayStatus !== "triggering" ? '<div class="replay-status">' + escapeHTML(replayStatus) + '</div>' : "") +
      '</div>';
    }

    function bindReplayControls(project) {
      const eventSelect = document.getElementById("replay-event");
      const refSelect = document.getElementById("replay-ref");
      const trigger = document.getElementById("replay-trigger");
      if (eventSelect) {
        eventSelect.addEventListener("change", () => {
          selectedReplayEvent = eventSelect.value;
          replayStatus = "";
          renderProjectDetail();
        });
      }
      if (refSelect) {
        refSelect.addEventListener("change", () => {
          selectedReplayRef = refSelect.value;
          replayStatus = "";
          renderProjectDetail();
        });
      }
      if (trigger) {
        trigger.addEventListener("click", () => triggerReplay(project));
      }
    }

    function triggerReplay(project) {
      if (!project || !selectedReplayEvent || !selectedReplayRef) return;
      replayStatus = "triggering";
      renderProjectDetail();
      fetch("/replay", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          repo: project.repo,
          event_key: selectedReplayEvent,
          ref: selectedReplayRef
        })
      }).then(resp => {
        if (!resp.ok) return resp.text().then(text => { throw new Error(text || "trigger failed"); });
        return resp.json();
      }).then(data => {
        replayStatus = data.delivery_id ? "Triggered " + data.delivery_id : "Triggered";
        refreshSnapshot();
      }).catch(err => {
        replayStatus = String(err.message || err || "trigger failed").trim();
      }).finally(() => {
        renderProjectDetail();
      });
    }

    function renderHooks(event) {
      return event.hooks && event.hooks.length ? event.hooks.map(hook =>
        '<article class="hook">' +
          '<div class="hook-title">' +
            '<span>' + escapeHTML(hook.name) + '</span>' +
            '<span class="pill ' + stateClass(hook.state) + '">' + escapeHTML(hook.state) + '</span>' +
          '</div>' +
          '<pre>' + escapeHTML(hook.output || "No output.") + '</pre>' +
        '</article>'
      ).join("") : '<div class="empty">No hooks recorded for this event.</div>';
    }

    function renderConfiguredEvents(project) {
      const events = project && project.configured_events ? project.configured_events : [];
      return events.length ? events.map(event =>
        '<article class="hook">' +
          '<div class="hook-title">' +
            '<span>' + escapeHTML(event.event_key) + '</span>' +
            '<span class="pill ' + stateClass(event.state) + '">' + escapeHTML(event.state || "no_runs") + '</span>' +
          '</div>' +
          '<div class="history-body">' +
            '<div class="meta">Source: ' + escapeHTML(event.source || "configured") + '</div>' +
            (event.ref ? '<div class="meta">Ref: ' + escapeHTML(event.ref) + '</div>' : "") +
            (event.hash ? '<div class="meta">Hash: ' + escapeHTML(event.hash) + '</div>' : "") +
            (event.updated_at ? '<div class="meta">Updated: ' + escapeHTML(fmtDate(event.updated_at)) + '</div>' : "") +
          '</div>' +
          '<pre>' + escapeHTML(event.latest_output || "No output.") + '</pre>' +
        '</article>'
      ).join("") : '<div class="empty">No configured outputs for this project.</div>';
    }

    function projectSummary(project) {
      const events = configuredEvents(project);
      if (events.length) {
        const completed = events.filter(event => event.state && event.state !== "no_runs").length;
        return completed + ' of ' + events.length + ' configured output' + (events.length === 1 ? '' : 's');
      }
      return project.loaded ? "no configured outputs" : "not loaded";
    }

    function render() {
      const activeRows = sortedActiveConfiguredEvents();
      const existingRows = sortedExistingProjects();

      activeEl.innerHTML = activeRows.length ? activeRows.map(row =>
        '<button class="event" type="button" data-repo="' + escapeHTML(row.project.repo) + '" aria-selected="' + (row.project.repo === selectedRepo) + '">' +
          '<div class="event-head">' +
            '<div class="repo">' + escapeHTML(row.project.repo) + '</div>' +
            '<div class="pill running">running</div>' +
          '</div>' +
          '<div class="meta">' + escapeHTML(row.event.event_key) + '</div>' +
          '<div class="meta">' + escapeHTML(row.event.latest_output || row.event.description || "No output yet.") + '</div>' +
        '</button>'
      ).join("") : '<div class="empty">No active projects.</div>';

      existingEl.innerHTML = existingRows.length ? existingRows.map(project =>
        '<button class="event" type="button" data-repo="' + escapeHTML(project.repo) + '" aria-selected="' + (project.repo === selectedRepo) + '">' +
          '<div class="event-head">' +
            '<div class="repo">' + escapeHTML(project.repo) + '</div>' +
            '<div class="pill ' + stateClass((latestConfiguredEvent(project) || {}).state) + '">' + escapeHTML((latestConfiguredEvent(project) || {}).state || "known") + '</div>' +
          '</div>' +
          '<div class="meta">' + escapeHTML(projectSummary(project)) + '</div>' +
          (latestConfiguredEvent(project) ? '<div class="meta">' + escapeHTML(latestConfiguredEvent(project).event_key) + '</div>' : "") +
        '</button>'
      ).join("") : '<div class="empty">' + (projectSearch ? 'No matching projects.' : 'No projects yet.') + '</div>';

      document.querySelectorAll("button[data-repo]").forEach(button => {
        button.addEventListener("click", () => {
          if (selectedRepo !== button.dataset.repo) {
            selectedReplayEvent = "";
            selectedReplayRef = "";
            replayStatus = "";
          }
          selectedRepo = button.dataset.repo;
          loadProjectDetail(selectedRepo);
          ensureReplayRefs(selectedRepo);
          render();
        });
      });

      if (!selectedRepo && existingRows.length) {
        selectedRepo = existingRows[0].repo;
        loadProjectDetail(selectedRepo);
        ensureReplayRefs(selectedRepo);
      }

      renderProjectDetail();
    }

    function renderProjectDetail() {
      const project = selectedProjectDetail && selectedProjectDetail.repo === selectedRepo ? selectedProjectDetail : existingProjects.get(selectedRepo);
      if (!project) {
        detailEl.innerHTML = '<div class="empty">Select a project to inspect configured outputs.</div>';
        return;
      }
      detailEl.innerHTML =
        '<div class="detail-head">' +
          '<h2>' + escapeHTML(project.repo) + '</h2>' +
          renderReplayControls(project) +
        '</div>' +
        '<div class="toolbar">' +
          '<span>' + escapeHTML(projectSummary(project)) + '</span>' +
          '<span class="pill ' + stateClass((latestConfiguredEvent(project) || {}).state) + '">' + escapeHTML((latestConfiguredEvent(project) || {}).state || "known") + '</span>' +
        '</div>' +
        (project.ref || project.hash ?
          '<article class="history-event"><div class="history-body">' +
            (project.ref ? '<div class="meta">Ref: ' + escapeHTML(project.ref) + '</div>' : "") +
            (project.hash ? '<div class="meta">Hash: ' + escapeHTML(project.hash) + '</div>' : "") +
            (project.updated_at ? '<div class="meta">Updated: ' + escapeHTML(fmtDate(project.updated_at)) + '</div>' : "") +
          '</div></article>' : "") +
        renderConfiguredEvents(project);
      bindReplayControls(project);
    }

    function renderEventQueue() {
      const rows = sortedQueueEvents();
      eventQueueEl.innerHTML = rows.length ? rows.map(event =>
        '<div class="queue-row">' +
          '<div><div class="repo">' + escapeHTML(event.repo) + '</div><div class="meta">' + escapeHTML(fmtDate(event.updated_at || event.started_at)) + '</div></div>' +
          '<div class="meta">' + eventLabel(event) + '</div>' +
          '<div><span class="pill ' + stateClass(event.state) + '">' + escapeHTML(event.state || "received") + '</span></div>' +
          '<div class="meta">' + escapeHTML(event.delivery_id || "") + '</div>' +
        '</div>'
      ).join("") : '<div class="empty">No events received yet.</div>';
    }

    function loadProjects() {
      return fetch("/projects").then(resp => resp.ok ? resp.json() : []).then(data => {
        const seen = new Set();
        data.forEach(repo => {
          if (typeof repo !== "string") return;
          seen.add(repo);
          if (!existingProjects.has(repo)) {
            existingProjects.set(repo, { repo: repo, state: "known" });
          }
        });
        [...existingProjects.keys()].forEach(repo => {
          if (!seen.has(repo)) existingProjects.delete(repo);
        });
        const loads = data.filter(repo => typeof repo === "string").map(repo => loadProjectDetail(repo, false));
        return Promise.all(loads).then(() => render());
      }).catch(() => render());
    }

    function loadProjectDetail(repo, shouldRender = true) {
      if (!repo) return Promise.resolve();
      return fetch("/projects/" + repo.split("/").map(encodeURIComponent).join("/")).then(resp => {
        if (!resp.ok) throw new Error("project not found");
        return resp.json();
      }).then(project => {
        project.loaded = true;
        existingProjects.set(project.repo, project);
        if (project.repo === selectedRepo) selectedProjectDetail = project;
        if (shouldRender) render();
      }).catch(() => {
        if (repo === selectedRepo) selectedProjectDetail = existingProjects.get(repo) || null;
        if (shouldRender) render();
      });
    }

    function scheduleProjectRefresh() {
      if (projectRefreshTimer) return;
      projectRefreshTimer = setTimeout(() => {
        projectRefreshTimer = null;
        loadProjects();
        if (selectedRepo) loadProjectDetail(selectedRepo);
      }, 750);
    }

    function loadEvents() {
      return fetch("/events").then(resp => resp.ok ? resp.json() : []).then(data => {
        data.forEach(upsertQueueEvent);
      });
    }

    function refreshSnapshot() {
      return Promise.all([
        loadEvents().catch(() => {}),
        loadProjects()
      ]).then(() => {
        if (selectedRepo) loadProjectDetail(selectedRepo);
      });
    }

    searchEl.addEventListener("input", () => {
      projectSearch = searchEl.value;
      render();
    });

    refreshSnapshot().then(() => render());

    const source = new EventSource("/events/stream");
    source.addEventListener("open", () => {
      connectionEl.textContent = "live";
      const now = Date.now();
      if (now - lastOpenRefresh > 1000) {
        lastOpenRefresh = now;
        refreshSnapshot();
      }
    });
    source.addEventListener("error", () => connectionEl.textContent = "reconnecting");
    source.addEventListener("event", msg => upsertQueueEvent(JSON.parse(msg.data)));
  </script>
</body>
</html>`)))
