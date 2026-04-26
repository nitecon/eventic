package client

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

const (
	defaultStateDBPath = "/opt/eventic/state/eventic.db"
)

// StateConfig controls the client-local persistent project database.
type StateConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// ProjectState is the persisted latest state for a managed repository.
type ProjectState struct {
	Repo             string            `json:"repo"`
	Ref              string            `json:"ref,omitempty"`
	Hash             string            `json:"hash,omitempty"`
	Event            string            `json:"event,omitempty"`
	Action           string            `json:"action,omitempty"`
	DeliveryID       string            `json:"delivery_id,omitempty"`
	State            string            `json:"state"`
	LatestOutput     string            `json:"latest_output,omitempty"`
	Description      string            `json:"description,omitempty"`
	StartedAt        time.Time         `json:"started_at"`
	FinishedAt       *time.Time        `json:"finished_at,omitempty"`
	UpdatedAt        time.Time         `json:"updated_at"`
	DurationMillis   int64             `json:"duration_ms,omitempty"`
	RecentEvents     []ProjectEvent    `json:"recent_events,omitempty"`
	ConfiguredEvents []ConfiguredEvent `json:"configured_events,omitempty"`
}

// ProjectEvent is a bounded persisted execution history item for one repo.
type ProjectEvent struct {
	Repo           string     `json:"repo"`
	DeliveryID     string     `json:"delivery_id"`
	Ref            string     `json:"ref,omitempty"`
	Hash           string     `json:"hash,omitempty"`
	Event          string     `json:"event,omitempty"`
	Action         string     `json:"action,omitempty"`
	State          string     `json:"state"`
	LatestOutput   string     `json:"latest_output,omitempty"`
	Description    string     `json:"description,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DurationMillis int64      `json:"duration_ms,omitempty"`
}

// ConfiguredEvent is a configured hook slot for a repo, with latest run output.
type ConfiguredEvent struct {
	Repo           string     `json:"repo"`
	EventKey       string     `json:"event_key"`
	Source         string     `json:"source"`
	Hook           string     `json:"hook"`
	Event          string     `json:"event,omitempty"`
	Action         string     `json:"action,omitempty"`
	DeliveryID     string     `json:"delivery_id,omitempty"`
	State          string     `json:"state"`
	LatestOutput   string     `json:"latest_output"`
	Description    string     `json:"description,omitempty"`
	Ref            string     `json:"ref,omitempty"`
	Hash           string     `json:"hash,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DurationMillis int64      `json:"duration_ms,omitempty"`
}

type ProjectStore struct {
	db *sql.DB
}

func OpenProjectStore(ctx context.Context, cfg StateConfig) (*ProjectStore, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	path := cfg.Path
	if path == "" {
		path = defaultStateDBPath
	}

	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("create state db directory: %w", err)
	}

	return openProjectStore(ctx, path)
}

func OpenMemoryProjectStore(ctx context.Context) (*ProjectStore, error) {
	return openProjectStore(ctx, "file:eventic-project-state?mode=memory&cache=shared")
}

func openProjectStore(ctx context.Context, path string) (*ProjectStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	db.SetMaxOpenConns(1)

	store := &ProjectStore{db: db}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *ProjectStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *ProjectStore) SeedFromReposDir(ctx context.Context, reposDir string, cfg Config) {
	if s == nil || reposDir == "" {
		return
	}

	started := time.Now()
	seeded := 0
	err := filepath.WalkDir(reposDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if entry.Name() == ".git" {
			return filepath.SkipDir
		}

		gitPath := filepath.Join(path, ".git")
		info, err := os.Stat(gitPath)
		if err != nil || !info.IsDir() {
			return nil
		}

		repo, err := filepath.Rel(reposDir, path)
		if err != nil {
			return filepath.SkipDir
		}
		repo = filepath.ToSlash(repo)
		if repo == "." || strings.HasPrefix(repo, "..") {
			return filepath.SkipDir
		}

		ref, hash, err := CurrentGitState(path)
		if err != nil {
			log.Warn().Err(err).Str("repo", repo).Msg("failed to seed project state")
			return filepath.SkipDir
		}
		s.UpsertManagedProject(ctx, repo, ref, hash)
		s.SyncConfiguredEvents(ctx, repo, path, cfg)
		seeded++
		return filepath.SkipDir
	})
	if err != nil {
		log.Warn().Err(err).Str("repos_dir", reposDir).Msg("failed to seed project state from repos dir")
		return
	}
	log.Info().Str("repos_dir", reposDir).Int("projects", seeded).Dur("duration", time.Since(started)).Msg("seeded project state from repos dir")
}

func (s *ProjectStore) UpsertManagedProject(ctx context.Context, repo, ref, hash string) {
	if s == nil {
		return
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (
			repo, ref, hash, state, started_at, updated_at
		)
		VALUES (?, ?, ?, 'known', ?, ?)
		ON CONFLICT(repo) DO UPDATE SET
			ref = excluded.ref,
			hash = excluded.hash,
			updated_at = excluded.updated_at
	`, repo, ref, hash, now, now)
	if err != nil {
		logStateError(err, "upsert managed project")
	}
}

func (s *ProjectStore) StartProject(ctx context.Context, event ExecutionEvent) {
	if s == nil {
		return
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (
			repo, ref, event, action, delivery_id, state,
			started_at, finished_at, updated_at, duration_ms
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, 0)
		ON CONFLICT(repo) DO UPDATE SET
			ref = excluded.ref,
			event = excluded.event,
			action = excluded.action,
			delivery_id = excluded.delivery_id,
			state = excluded.state,
			started_at = excluded.started_at,
			finished_at = NULL,
			updated_at = excluded.updated_at,
			duration_ms = 0
	`, event.Repo, event.Ref, event.Event, event.Action, event.DeliveryID, event.State, event.StartedAt, now)
	if err != nil {
		logStateError(err, "start project state")
	}
	s.RecordProjectEvent(ctx, event)
}

func (s *ProjectStore) SyncConfiguredEvents(ctx context.Context, repo, repoPath string, cfg Config) {
	if s == nil {
		return
	}
	now := time.Now()
	configured := configuredEventsForRepo(repo, repoPath, cfg)
	if len(configured) == 0 {
		if repoPath != "" {
			_, err := s.db.ExecContext(ctx, `DELETE FROM configured_events WHERE repo = ?`, repo)
			if err != nil {
				logStateError(err, "clear configured events")
			}
		}
		return
	}

	keys := make([]string, 0, len(configured))
	for _, event := range configured {
		keys = append(keys, event.EventKey)
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO configured_events (
				repo, event_key, source, hook, event, action, state, latest_output, description, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, 'no_runs', '', '', ?)
			ON CONFLICT(repo, event_key) DO UPDATE SET
				source = excluded.source,
				hook = excluded.hook,
				event = excluded.event,
				action = excluded.action,
				updated_at = excluded.updated_at
		`, repo, event.EventKey, event.Source, event.Hook, event.Event, event.Action, now)
		if err != nil {
			logStateError(err, "sync configured event")
		}
	}

	if repoPath != "" {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
		args := make([]any, 0, len(keys)+1)
		args = append(args, repo)
		for _, key := range keys {
			args = append(args, key)
		}
		_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
			DELETE FROM configured_events
			WHERE repo = ? AND event_key NOT IN (%s)
		`, placeholders), args...)
		if err != nil {
			logStateError(err, "prune configured events")
		}
	}
}

func (s *ProjectStore) UpdateGitState(ctx context.Context, repo, ref, hash string) {
	if s == nil {
		return
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET ref = COALESCE(NULLIF(?, ''), ref),
			hash = ?,
			updated_at = ?
		WHERE repo = ?
	`, ref, hash, time.Now(), repo)
	if err != nil {
		logStateError(err, "update git state")
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE project_events
		SET ref = COALESCE(NULLIF(?, ''), ref),
			hash = ?,
			updated_at = ?
		WHERE repo = ?
			AND delivery_id = (SELECT delivery_id FROM projects WHERE repo = ?)
	`, ref, hash, time.Now(), repo, repo)
	if err != nil {
		logStateError(err, "update project event git state")
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE configured_events
		SET ref = COALESCE(NULLIF(?, ''), ref),
			hash = ?,
			updated_at = ?
		WHERE repo = ?
	`, ref, hash, time.Now(), repo)
	if err != nil {
		logStateError(err, "update configured event git state")
	}
}

func (s *ProjectStore) UpdateOutput(ctx context.Context, repo string, event protocol.EventMsg, hookName, state, output string) {
	if s == nil {
		return
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET state = ?,
			latest_output = ?,
			updated_at = ?
		WHERE repo = ?
	`, state, output, time.Now(), repo)
	if err != nil {
		logStateError(err, "update project output")
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE project_events
		SET state = ?,
			latest_output = ?,
			updated_at = ?
		WHERE repo = ?
			AND delivery_id = (SELECT delivery_id FROM projects WHERE repo = ?)
	`, state, output, time.Now(), repo, repo)
	if err != nil {
		logStateError(err, "update project event output")
	}
	s.UpdateConfiguredEvent(ctx, repo, event, hookName, state, output, "")
}

func (s *ProjectStore) FinishProject(ctx context.Context, event ExecutionEvent) {
	if s == nil {
		return
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET state = ?,
			description = ?,
			finished_at = ?,
			updated_at = ?,
			duration_ms = ?
		WHERE repo = ?
	`, event.State, event.Description, event.FinishedAt, event.UpdatedAt, event.DurationMillis, event.Repo)
	if err != nil {
		logStateError(err, "finish project state")
	}
	s.RecordProjectEvent(ctx, event)
}

func (s *ProjectStore) ListProjects(ctx context.Context) ([]string, error) {
	if s == nil {
		return []string{}, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT repo
		FROM projects
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, err
		}
		projects = append(projects, repo)
	}
	return projects, rows.Err()
}

func (s *ProjectStore) GetProject(ctx context.Context, repo string) (*ProjectState, error) {
	if s == nil {
		return nil, sql.ErrNoRows
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT repo, ref, hash, event, action, delivery_id, state,
			latest_output, description, started_at, finished_at, updated_at, duration_ms
		FROM projects
		WHERE repo = ?
	`, repo)
	project, err := scanProject(row)
	if err != nil {
		return nil, err
	}
	project.ConfiguredEvents, err = s.ListConfiguredEvents(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &project, nil
}

func (s *ProjectStore) StartConfiguredEvent(ctx context.Context, repo string, event protocol.EventMsg, hookName string) {
	if s == nil {
		return
	}
	keys := configuredEventKeys(event, hookName)
	if len(keys) == 0 {
		return
	}
	now := time.Now()
	for _, key := range keys {
		res, err := s.db.ExecContext(ctx, `
			UPDATE configured_events
			SET delivery_id = ?,
				state = 'running',
				description = '',
				ref = ?,
				started_at = ?,
				finished_at = NULL,
				updated_at = ?,
				duration_ms = 0
			WHERE repo = ? AND event_key = ?
		`, event.DeliveryID, event.Ref, now, now, repo, key)
		if err != nil {
			logStateError(err, "start configured event")
			return
		}
		if rows, _ := res.RowsAffected(); rows > 0 {
			return
		}
	}
}

func (s *ProjectStore) UpdateConfiguredEvent(ctx context.Context, repo string, event protocol.EventMsg, hookName, state, output, desc string) {
	if s == nil {
		return
	}
	keys := configuredEventKeys(event, hookName)
	if len(keys) == 0 {
		return
	}
	now := time.Now()
	for _, key := range keys {
		res, err := s.db.ExecContext(ctx, `
			UPDATE configured_events
			SET delivery_id = ?,
				state = ?,
				latest_output = ?,
				description = ?,
				ref = ?,
				started_at = ?,
				finished_at = ?,
				updated_at = ?,
				duration_ms = ?
			WHERE repo = ? AND event_key = ?
		`, event.DeliveryID, state, output, desc, event.Ref, now, now, now, 0, repo, key)
		if err != nil {
			logStateError(err, "update configured event")
			return
		}
		if rows, _ := res.RowsAffected(); rows > 0 {
			return
		}
	}
}

func (s *ProjectStore) ListConfiguredEvents(ctx context.Context, repo string) ([]ConfiguredEvent, error) {
	if s == nil {
		return []ConfiguredEvent{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo, event_key, source, hook, event, action, delivery_id, state,
			latest_output, description, ref, hash, started_at, finished_at, updated_at, duration_ms
		FROM configured_events
		WHERE repo = ?
		ORDER BY event_key
	`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []ConfiguredEvent
	for rows.Next() {
		event, err := scanConfiguredEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *ProjectStore) RecordProjectEvent(ctx context.Context, event ExecutionEvent) {
	if s == nil {
		return
	}
	output := latestEventOutput(event)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_events (
			repo, delivery_id, ref, event, action, state,
			latest_output, description, started_at, finished_at, updated_at, duration_ms
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo, delivery_id) DO UPDATE SET
			ref = excluded.ref,
			event = excluded.event,
			action = excluded.action,
			state = excluded.state,
			latest_output = excluded.latest_output,
			description = excluded.description,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			updated_at = excluded.updated_at,
			duration_ms = excluded.duration_ms
	`, event.Repo, event.DeliveryID, event.Ref, event.Event, event.Action, event.State, output, event.Description, event.StartedAt, event.FinishedAt, event.UpdatedAt, event.DurationMillis)
	if err != nil {
		logStateError(err, "record project event")
		return
	}
	s.PruneProjectEvents(ctx, event.Repo, 5)
}

func (s *ProjectStore) ListProjectEvents(ctx context.Context, repo string, limit int) ([]ProjectEvent, error) {
	if s == nil {
		return []ProjectEvent{}, nil
	}
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT repo, delivery_id, ref, hash, event, action, state,
			latest_output, description, started_at, finished_at, updated_at, duration_ms
		FROM project_events
		WHERE repo = ?
		ORDER BY updated_at DESC
		LIMIT ?
	`, repo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []ProjectEvent
	for rows.Next() {
		event, err := scanProjectEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *ProjectStore) PruneProjectEvents(ctx context.Context, repo string, retain int) {
	if s == nil {
		return
	}
	if retain <= 0 {
		retain = 5
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM project_events
		WHERE repo = ?
			AND delivery_id NOT IN (
				SELECT delivery_id
				FROM project_events
				WHERE repo = ?
				ORDER BY updated_at DESC
				LIMIT ?
			)
	`, repo, repo, retain)
	if err != nil {
		logStateError(err, "prune project events")
	}
}

func (s *ProjectStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS projects (
			repo TEXT PRIMARY KEY,
			ref TEXT NOT NULL DEFAULT '',
			hash TEXT NOT NULL DEFAULT '',
			event TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			delivery_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT '',
			latest_output TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			started_at DATETIME NOT NULL,
			finished_at DATETIME,
			updated_at DATETIME NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_projects_updated_at ON projects(updated_at DESC);
		CREATE TABLE IF NOT EXISTS project_events (
			repo TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			ref TEXT NOT NULL DEFAULT '',
			hash TEXT NOT NULL DEFAULT '',
			event TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT '',
			latest_output TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			started_at DATETIME NOT NULL,
			finished_at DATETIME,
			updated_at DATETIME NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (repo, delivery_id)
		);
		CREATE INDEX IF NOT EXISTS idx_project_events_repo_updated_at ON project_events(repo, updated_at DESC);
		CREATE TABLE IF NOT EXISTS configured_events (
			repo TEXT NOT NULL,
			event_key TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			hook TEXT NOT NULL DEFAULT '',
			event TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			delivery_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'no_runs',
			latest_output TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			ref TEXT NOT NULL DEFAULT '',
			hash TEXT NOT NULL DEFAULT '',
			started_at DATETIME,
			finished_at DATETIME,
			updated_at DATETIME NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (repo, event_key)
		);
		CREATE INDEX IF NOT EXISTS idx_configured_events_repo ON configured_events(repo, event_key);
	`)
	if err != nil {
		return fmt.Errorf("migrate state db: %w", err)
	}
	return nil
}

type projectScanner interface {
	Scan(dest ...any) error
}

func scanProject(scanner projectScanner) (ProjectState, error) {
	var project ProjectState
	var finishedAt sql.NullTime
	err := scanner.Scan(
		&project.Repo,
		&project.Ref,
		&project.Hash,
		&project.Event,
		&project.Action,
		&project.DeliveryID,
		&project.State,
		&project.LatestOutput,
		&project.Description,
		&project.StartedAt,
		&finishedAt,
		&project.UpdatedAt,
		&project.DurationMillis,
	)
	if err != nil {
		return ProjectState{}, err
	}
	if finishedAt.Valid {
		project.FinishedAt = &finishedAt.Time
	}
	return project, nil
}

func scanProjectEvent(scanner projectScanner) (ProjectEvent, error) {
	var event ProjectEvent
	var finishedAt sql.NullTime
	err := scanner.Scan(
		&event.Repo,
		&event.DeliveryID,
		&event.Ref,
		&event.Hash,
		&event.Event,
		&event.Action,
		&event.State,
		&event.LatestOutput,
		&event.Description,
		&event.StartedAt,
		&finishedAt,
		&event.UpdatedAt,
		&event.DurationMillis,
	)
	if err != nil {
		return ProjectEvent{}, err
	}
	if finishedAt.Valid {
		event.FinishedAt = &finishedAt.Time
	}
	return event, nil
}

func scanConfiguredEvent(scanner projectScanner) (ConfiguredEvent, error) {
	var event ConfiguredEvent
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	err := scanner.Scan(
		&event.Repo,
		&event.EventKey,
		&event.Source,
		&event.Hook,
		&event.Event,
		&event.Action,
		&event.DeliveryID,
		&event.State,
		&event.LatestOutput,
		&event.Description,
		&event.Ref,
		&event.Hash,
		&startedAt,
		&finishedAt,
		&event.UpdatedAt,
		&event.DurationMillis,
	)
	if err != nil {
		return ConfiguredEvent{}, err
	}
	if startedAt.Valid {
		event.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		event.FinishedAt = &finishedAt.Time
	}
	return event, nil
}

func latestEventOutput(event ExecutionEvent) string {
	for i := len(event.Hooks) - 1; i >= 0; i-- {
		if event.Hooks[i].Output != "" {
			return event.Hooks[i].Output
		}
	}
	return ""
}

func configuredEventsForRepo(repo, repoPath string, cfg Config) []ConfiguredEvent {
	var events []ConfiguredEvent
	add := func(source, rawKey, hook string) {
		eventName, action := splitConfiguredEventKey(rawKey)
		key := configuredEventKey(rawKey, hook)
		events = append(events, ConfiguredEvent{
			Repo:     repo,
			EventKey: key,
			Source:   source,
			Hook:     hook,
			Event:    eventName,
			Action:   action,
			State:    "no_runs",
		})
	}

	if repoPath == "" {
		addClientGlobalConfiguredEvents(cfg, add)
		return events
	}

	configPath := filepath.Join(repoPath, ".eventic.yaml")
	if data, err := os.ReadFile(configPath); err == nil {
		var repoCfg EventicConfig
		if err := yaml.Unmarshal(data, &repoCfg); err == nil {
			if repoCfg.Hooks.Pre != "" {
				add(".eventic.yaml", "global", "pre")
			}
			if repoCfg.Hooks.Post != "" {
				add(".eventic.yaml", "global", "post")
			}
			if repoCfg.Hooks.Notify != "" {
				add(".eventic.yaml", "global", "notify")
			}
			for rawKey, hooks := range repoCfg.Events {
				if hooks.Pre != "" {
					add(".eventic.yaml", rawKey, "pre")
				}
				if hooks.Post != "" {
					add(".eventic.yaml", rawKey, "post")
				}
				if hooks.Notify != "" {
					add(".eventic.yaml", rawKey, "notify")
				}
			}
			return events
		}
	}

	deployPath := filepath.Join(repoPath, ".deploy", "deploy.yml")
	if _, err := os.Stat(deployPath); err == nil {
		add(".deploy/deploy.yml", "push", "post")
		return events
	}

	addClientGlobalConfiguredEvents(cfg, add)
	return events
}

func addClientGlobalConfiguredEvents(cfg Config, add func(source, rawKey, hook string)) {
	if cfg.GlobalHooks.Pre != "" {
		add("client-global", "global", "pre")
	}
	if cfg.GlobalHooks.Post != "" {
		add("client-global", "global", "post")
	}
	if cfg.GlobalHooks.Notify != "" {
		add("client-global", "global", "notify")
	}
}

func configuredEventKeys(event protocol.EventMsg, hookName string) []string {
	var hook string
	switch hookName {
	case "global:pre":
		return []string{"global:pre"}
	case "global:post":
		return []string{"global:post"}
	case "global:summary":
		return []string{"global:notify"}
	case "event:pre":
		hook = "pre"
	case "event:post":
		hook = "post"
	case "event:notify":
		hook = "notify"
	default:
		return nil
	}

	keys := []string{}
	if event.Action != "" {
		keys = append(keys, configuredEventKey(event.GitHubEvent+"."+event.Action, hook))
	}
	keys = append(keys, configuredEventKey(event.GitHubEvent, hook))
	return keys
}

func configuredEventKey(rawKey, hook string) string {
	return rawKey + ":" + hook
}

func splitConfiguredEventKey(rawKey string) (string, string) {
	if rawKey == "global" {
		return "", ""
	}
	parts := strings.SplitN(rawKey, ".", 2)
	if len(parts) == 1 {
		return rawKey, ""
	}
	return parts[0], parts[1]
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

func logStateError(err error, msg string) {
	log.Error().Err(err).Msg(msg)
}
