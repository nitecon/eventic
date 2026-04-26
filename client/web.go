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

func (l *ExecutionLog) StartEvent(event protocol.EventMsg) {
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
}

func (l *ExecutionLog) FinishEvent(deliveryID, state, desc string) {
	l.updateEvent(deliveryID, func(rec *ExecutionEvent) {
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

func (l *ExecutionLog) updateEvent(deliveryID string, mutate func(*ExecutionEvent)) {
	l.mu.Lock()
	idx, ok := l.byDeliveryID[deliveryID]
	if !ok {
		l.mu.Unlock()
		return
	}
	mutate(&l.events[idx])
	snapshot := cloneEventLocked(l.events[idx])
	l.mu.Unlock()

	l.publish(snapshot)
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

func StartWebConsole(ctx context.Context, cfg WebConfig, logStore *ExecutionLog) error {
	listen := cfg.Listen
	if listen == "" {
		listen = defaultWebListen
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", webIndexHandler)
	mux.HandleFunc("/events", eventsHandler(logStore))
	mux.HandleFunc("/events/stream", eventsStreamHandler(logStore))
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
    .events {
      border-right: 1px solid var(--border);
      background: var(--panel);
      overflow: auto;
      max-height: calc(100vh - 56px);
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
      .events {
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
    <section class="events" id="events">
      <div class="empty">No events yet.</div>
    </section>
    <section class="detail" id="detail">
      <div class="empty">Select an event to inspect hook output.</div>
    </section>
  </main>
  <script>
    const events = new Map();
    let selected = "";
    const listEl = document.getElementById("events");
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
      events.set(event.delivery_id, event);
      if (!selected) selected = event.delivery_id;
      render();
    }

    function sortedEvents() {
      return [...events.values()].sort((a, b) =>
        new Date(b.started_at).getTime() - new Date(a.started_at).getTime()
      );
    }

    function render() {
      const rows = sortedEvents();
      if (rows.length === 0) {
        listEl.innerHTML = '<div class="empty">No events yet.</div>';
        detailEl.innerHTML = '<div class="empty">Select an event to inspect hook output.</div>';
        return;
      }
      listEl.innerHTML = rows.map(event =>
        '<button class="event" type="button" data-id="' + escapeHTML(event.delivery_id) + '" aria-selected="' + (event.delivery_id === selected) + '">' +
          '<div class="event-head">' +
            '<div class="repo">' + escapeHTML(event.repo) + '</div>' +
            '<div class="pill ' + stateClass(event.state) + '">' + escapeHTML(event.state) + '</div>' +
          '</div>' +
          '<div class="meta">' + escapeHTML(event.event) + (event.action ? "." + escapeHTML(event.action) : "") + '</div>' +
          '<div class="meta">' + escapeHTML(fmtDate(event.started_at)) + '</div>' +
        '</button>'
      ).join("");
      listEl.querySelectorAll("button[data-id]").forEach(button => {
        button.addEventListener("click", () => {
          selected = button.dataset.id;
          render();
        });
      });

      const event = events.get(selected) || rows[0];
      selected = event.delivery_id;
      const hooks = event.hooks && event.hooks.length ? event.hooks.map(hook =>
        '<article class="hook">' +
          '<div class="hook-title">' +
            '<span>' + escapeHTML(hook.name) + '</span>' +
            '<span class="pill ' + stateClass(hook.state) + '">' + escapeHTML(hook.state) + '</span>' +
          '</div>' +
          '<pre>' + escapeHTML(hook.output || "No output.") + '</pre>' +
        '</article>'
      ).join("") : '<div class="empty">No hooks recorded for this event.</div>';
      detailEl.innerHTML =
        '<h2>' + escapeHTML(event.repo) + '</h2>' +
        '<div class="toolbar">' +
          '<span>' + escapeHTML(event.event) + (event.action ? "." + escapeHTML(event.action) : "") + '</span>' +
          '<span>' + (event.duration_ms ? escapeHTML(event.duration_ms + " ms") : "running") + '</span>' +
        '</div>' +
        '<div class="meta">Delivery: ' + escapeHTML(event.delivery_id) + '</div>' +
        (event.ref ? '<div class="meta">Ref: ' + escapeHTML(event.ref) + '</div>' : "") +
        (event.description ? '<div class="meta">Description: ' + escapeHTML(event.description) + '</div>' : "") +
        hooks;
    }

    fetch("/events").then(resp => resp.json()).then(data => {
      data.forEach(upsert);
      render();
    });

    const source = new EventSource("/events/stream");
    source.addEventListener("open", () => connectionEl.textContent = "live");
    source.addEventListener("error", () => connectionEl.textContent = "reconnecting");
    source.addEventListener("event", msg => upsert(JSON.parse(msg.data)));
  </script>
</body>
</html>`)))
