package client

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
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

func StartWebConsole(ctx context.Context, cfg WebConfig, logStore *ExecutionLog, projectStore *ProjectStore) error {
	listen := cfg.Listen
	if listen == "" {
		listen = defaultWebListen
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", webIndexHandler)
	mux.HandleFunc("/events", eventsHandler(logStore))
	mux.HandleFunc("/events/stream", eventsStreamHandler(logStore))
	mux.HandleFunc("/projects", projectsHandler(projectStore))
	mux.HandleFunc("/projects/", projectsHandler(projectStore))
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
      min-height: calc(100vh - 56px);
    }
    .sidebar {
      border-right: 1px solid var(--border);
      background: var(--panel);
      overflow: auto;
      max-height: calc(100vh - 56px);
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
      max-height: calc(100vh - 244px);
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
      max-height: calc(100vh - 56px);
    }
    .detail h2 {
      margin: 0 0 8px;
      font-size: 20px;
      overflow-wrap: anywhere;
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
    @media (max-width: 760px) {
      main { grid-template-columns: 1fr; }
      .sidebar {
        max-height: 45vh;
        border-right: 0;
        border-bottom: 1px solid var(--border);
      }
      .detail { max-height: none; }
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
      <div class="empty">Select a project to inspect recent events.</div>
    </section>
  </main>
  <script>
    const activeRepos = new Map();
    const existingProjects = new Map();
    const repoLimit = 50;
    let selectedRepo = "";
    let selectedMode = "existing";
    let selectedProjectDetail = null;
    let projectRefreshTimer = null;
    let projectSearch = "";
    let lastOpenRefresh = 0;
    const activeEl = document.getElementById("active-projects");
    const existingEl = document.getElementById("existing-projects");
    const searchEl = document.getElementById("project-search");
    const detailEl = document.getElementById("detail");
    const connectionEl = document.getElementById("connection");

    function stateClass(state) {
      return ["success", "failure", "running"].includes(state) ? state : "";
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

    function upsert(event) {
      let bucket = activeRepos.get(event.repo);
      if (!bucket) {
        bucket = { repo: event.repo, events: new Map(), latest: event };
        activeRepos.set(event.repo, bucket);
      }
      bucket.events.set(event.delivery_id, event);
      bucket.latest = latestOf([...bucket.events.values()]);
      const sorted = sortedEvents(bucket);
      while (sorted.length > repoLimit) {
        bucket.events.delete(sorted.pop().delivery_id);
      }
      bucket.latest = latestOf([...bucket.events.values()]);
      if (!selectedRepo) {
        selectedRepo = event.repo;
        selectedMode = "active";
      }
      render();
      scheduleProjectRefresh();
    }

    function latestOf(events) {
      return events.sort((a, b) =>
        new Date(b.updated_at || b.started_at).getTime() - new Date(a.updated_at || a.started_at).getTime()
      )[0];
    }

    function sortedEvents(bucket) {
      return [...bucket.events.values()].sort((a, b) =>
        new Date(b.started_at).getTime() - new Date(a.started_at).getTime()
      );
    }

    function sortedActiveRepos() {
      return [...activeRepos.values()].filter(bucket => bucket.latest && bucket.latest.state === "running").sort((a, b) =>
        new Date((b.latest && (b.latest.updated_at || b.latest.started_at)) || 0).getTime() -
        new Date((a.latest && (a.latest.updated_at || a.latest.started_at)) || 0).getTime()
      );
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

    function eventLabel(event) {
      return escapeHTML(event.event || "event") + (event.action ? "." + escapeHTML(event.action) : "");
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
      if (project.configured_events && project.configured_events.length) {
        const completed = project.configured_events.filter(event => event.state && event.state !== "no_runs").length;
        return completed + ' of ' + project.configured_events.length + ' configured output' + (project.configured_events.length === 1 ? '' : 's');
      }
      if (!project.updated_at) return "not loaded";
      return project.state && project.state !== "known" ? project.state : "no runs";
    }

    function render() {
      const activeRows = sortedActiveRepos();
      const existingRows = sortedExistingProjects();

      activeEl.innerHTML = activeRows.length ? activeRows.map(bucket => {
        const latest = bucket.latest;
        return '<button class="event" type="button" data-mode="active" data-repo="' + escapeHTML(bucket.repo) + '" aria-selected="' + (selectedMode === "active" && bucket.repo === selectedRepo) + '">' +
          '<div class="event-head">' +
            '<div class="repo">' + escapeHTML(bucket.repo) + '</div>' +
            '<div class="pill ' + stateClass(latest.state) + '">' + escapeHTML(latest.state) + '</div>' +
          '</div>' +
          '<div class="meta">' + eventLabel(latest) + '</div>' +
          '<div class="meta">' + bucket.events.size + ' recent event' + (bucket.events.size === 1 ? '' : 's') + '</div>' +
        '</button>';
      }).join("") : '<div class="empty">No active projects.</div>';

      existingEl.innerHTML = existingRows.length ? existingRows.map(project =>
        '<button class="event" type="button" data-mode="existing" data-repo="' + escapeHTML(project.repo) + '" aria-selected="' + (selectedMode === "existing" && project.repo === selectedRepo) + '">' +
          '<div class="event-head">' +
            '<div class="repo">' + escapeHTML(project.repo) + '</div>' +
            '<div class="pill ' + stateClass(project.state) + '">' + escapeHTML(project.state || "known") + '</div>' +
          '</div>' +
          '<div class="meta">' + escapeHTML(projectSummary(project)) + '</div>' +
          (project.ref ? '<div class="meta">' + escapeHTML(project.ref) + '</div>' : "") +
        '</button>'
      ).join("") : '<div class="empty">' + (projectSearch ? 'No matching projects.' : 'No projects yet.') + '</div>';

      document.querySelectorAll("button[data-repo]").forEach(button => {
        button.addEventListener("click", () => {
          selectedRepo = button.dataset.repo;
          selectedMode = button.dataset.mode;
          if (selectedMode === "existing") {
            loadProjectDetail(selectedRepo);
          }
          render();
        });
      });

      if (!selectedRepo && existingRows.length) {
        selectedRepo = existingRows[0].repo;
        selectedMode = "existing";
        loadProjectDetail(selectedRepo);
      }

      if (selectedMode === "existing") {
        renderProjectDetail();
      } else {
        renderActiveDetail(activeRows);
      }
    }

    function renderActiveDetail(activeRows) {
      const bucket = activeRepos.get(selectedRepo) || activeRows[0];
      if (!bucket) {
        detailEl.innerHTML = '<div class="empty">Select a project to inspect recent events.</div>';
        return;
      }
      selectedRepo = bucket.repo;
      const latest = bucket.latest;
      const history = sortedEvents(bucket);
      detailEl.innerHTML =
        '<h2>' + escapeHTML(bucket.repo) + '</h2>' +
        '<div class="toolbar">' +
          '<span>' + history.length + ' recent event' + (history.length === 1 ? '' : 's') + '</span>' +
          '<span class="pill ' + stateClass(latest.state) + '">' + escapeHTML(latest.state) + '</span>' +
        '</div>' +
        history.map(event =>
          '<article class="history-event">' +
            '<div class="history-title">' +
              '<div>' +
                '<div class="repo">' + eventLabel(event) + '</div>' +
                '<div class="meta">' + escapeHTML(fmtDate(event.started_at)) + '</div>' +
              '</div>' +
              '<span class="pill ' + stateClass(event.state) + '">' + escapeHTML(event.state) + '</span>' +
            '</div>' +
            '<div class="history-body">' +
              '<div class="meta">Delivery: ' + escapeHTML(event.delivery_id) + '</div>' +
              (event.ref ? '<div class="meta">Ref: ' + escapeHTML(event.ref) + '</div>' : "") +
              (event.duration_ms ? '<div class="meta">Duration: ' + escapeHTML(event.duration_ms + " ms") + '</div>' : "") +
              (event.description ? '<div class="meta">Description: ' + escapeHTML(event.description) + '</div>' : "") +
              renderHooks(event) +
            '</div>' +
          '</article>'
        ).join("");
    }

    function renderProjectDetail() {
      const project = selectedProjectDetail && selectedProjectDetail.repo === selectedRepo ? selectedProjectDetail : existingProjects.get(selectedRepo);
      if (!project) {
        detailEl.innerHTML = '<div class="empty">Select a project to inspect configured outputs.</div>';
        return;
      }
      detailEl.innerHTML =
        '<h2>' + escapeHTML(project.repo) + '</h2>' +
        '<div class="toolbar">' +
          '<span>' + escapeHTML(projectSummary(project)) + '</span>' +
          '<span class="pill ' + stateClass(project.state) + '">' + escapeHTML(project.state || "known") + '</span>' +
        '</div>' +
        (project.ref || project.hash ?
          '<article class="history-event"><div class="history-body">' +
            (project.ref ? '<div class="meta">Ref: ' + escapeHTML(project.ref) + '</div>' : "") +
            (project.hash ? '<div class="meta">Hash: ' + escapeHTML(project.hash) + '</div>' : "") +
            (project.updated_at ? '<div class="meta">Updated: ' + escapeHTML(fmtDate(project.updated_at)) + '</div>' : "") +
          '</div></article>' : "") +
        renderConfiguredEvents(project);
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
        if (selectedMode === "existing" && selectedRepo) {
          const current = existingProjects.get(selectedRepo);
          if (current && (!selectedProjectDetail || selectedProjectDetail.repo !== selectedRepo)) {
            selectedProjectDetail = current;
          }
        }
        render();
      }).catch(() => render());
    }

    function loadProjectDetail(repo) {
      if (!repo) return;
      fetch("/projects/" + repo.split("/").map(encodeURIComponent).join("/")).then(resp => {
        if (!resp.ok) throw new Error("project not found");
        return resp.json();
      }).then(project => {
        existingProjects.set(project.repo, project);
        selectedProjectDetail = project;
        render();
      }).catch(() => {
        selectedProjectDetail = existingProjects.get(repo) || null;
        render();
      });
    }

    function scheduleProjectRefresh() {
      if (projectRefreshTimer) return;
      projectRefreshTimer = setTimeout(() => {
        projectRefreshTimer = null;
        loadProjects();
        if (selectedMode === "existing") loadProjectDetail(selectedRepo);
      }, 750);
    }

    function loadEvents() {
      return fetch("/events").then(resp => resp.ok ? resp.json() : []).then(data => {
        data.forEach(upsert);
      });
    }

    function refreshSnapshot() {
      return Promise.all([
        loadEvents().catch(() => {}),
        loadProjects()
      ]).then(() => {
        if (selectedMode === "existing" && selectedRepo) loadProjectDetail(selectedRepo);
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
    source.addEventListener("event", msg => upsert(JSON.parse(msg.data)));
  </script>
</body>
</html>`)))
