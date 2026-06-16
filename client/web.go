package client

import (
	"context"
	"encoding/json"
	"fmt"
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
	// StaticDir, when set and containing an index.html, is served at "/" so the
	// user can drop a custom ndesign dashboard there. When empty a minimal
	// embedded shell is served instead.
	StaticDir string `yaml:"static_dir"`
	// Token, when set, gates the live WebSocket stream: clients must supply a
	// matching "?token=" query parameter. When empty the stream is open (the
	// localhost console default).
	Token string `yaml:"token"`
}

type ReplayDispatcher func(context.Context, protocol.EventMsg)

type ReplayRef struct {
	Ref  string `json:"ref"`
	Type string `json:"type"`
	Hash string `json:"hash,omitempty"`
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

	// Dashboard shell: a user-supplied static dir (ndesign markup) or the
	// embedded Go-template dashboard. The store is passed so the left-nav
	// hierarchy can be server-rendered on each request.
	mux.Handle("/", indexHandler(cfg, projectStore))

	// ── ndesign JSON API ─────────────────────────────────────────────────────
	mux.HandleFunc("/api/workflows", workflowsCollectionHandler(projectStore))
	mux.HandleFunc("/api/workflows/", workflowItemHandler(projectStore))
	mux.HandleFunc("/api/event-types", eventTypesHandler())
	mux.HandleFunc("/api/projects", apiProjectsHandler(projectStore))
	mux.HandleFunc("/api/projects/", apiProjectsHandler(projectStore))
	mux.HandleFunc("/api/refs", apiRefsHandler(cfg))
	mux.HandleFunc("/api/runs", runsCollectionHandler(projectStore, replay))
	mux.HandleFunc("/api/runs/", runItemHandler(projectStore))

	// SSE fallback for environments without WebSocket support.
	mux.HandleFunc("/api/runs/stream", eventsStreamHandler(logStore))

	// ── Live WebSocket stream (ndesign conventions) ──────────────────────────
	mux.HandleFunc("/ws/runs", wsRunsHandler(cfg.Web, logStore))

	// ── Back-compat aliases (pre-ndesign console) ────────────────────────────
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

// splitEventType splits an "event.action" key into its event and action parts.
// A key without a "." has an empty action.
func splitEventType(eventType string) (event, action string) {
	parts := strings.SplitN(strings.TrimSpace(eventType), ".", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
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
