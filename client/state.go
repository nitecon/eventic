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

	"github.com/rs/zerolog/log"
	_ "modernc.org/sqlite"
)

const (
	defaultStateDBPath         = "/opt/eventic/state/eventic.db"
	defaultProjectEventsRetain = 5
)

// StateConfig controls the client-local persistent project database.
type StateConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// ProjectState is the persisted latest state for a managed repository.
type ProjectState struct {
	Repo           string         `json:"repo"`
	Ref            string         `json:"ref,omitempty"`
	Hash           string         `json:"hash,omitempty"`
	Event          string         `json:"event,omitempty"`
	Action         string         `json:"action,omitempty"`
	DeliveryID     string         `json:"delivery_id,omitempty"`
	State          string         `json:"state"`
	LatestOutput   string         `json:"latest_output,omitempty"`
	Description    string         `json:"description,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	FinishedAt     *time.Time     `json:"finished_at,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DurationMillis int64          `json:"duration_ms,omitempty"`
	RecentEvents   []ProjectEvent `json:"recent_events,omitempty"`
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

func (s *ProjectStore) SeedFromReposDir(ctx context.Context, reposDir string) {
	if s == nil || reposDir == "" {
		return
	}

	err := filepath.WalkDir(reposDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() || entry.Name() != ".git" {
			return nil
		}

		repoPath := filepath.Dir(path)
		repo, err := filepath.Rel(reposDir, repoPath)
		if err != nil {
			return filepath.SkipDir
		}
		repo = filepath.ToSlash(repo)
		if repo == "." || strings.HasPrefix(repo, "..") {
			return filepath.SkipDir
		}

		ref, hash, err := CurrentGitState(repoPath)
		if err != nil {
			log.Warn().Err(err).Str("repo", repo).Msg("failed to seed project state")
			return filepath.SkipDir
		}
		s.UpsertManagedProject(ctx, repo, ref, hash)
		return filepath.SkipDir
	})
	if err != nil {
		log.Warn().Err(err).Str("repos_dir", reposDir).Msg("failed to seed project state from repos dir")
	}
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
}

func (s *ProjectStore) UpdateOutput(ctx context.Context, repo, state, output string) {
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

func (s *ProjectStore) ListProjects(ctx context.Context) ([]ProjectState, error) {
	if s == nil {
		return []ProjectState{}, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT repo, ref, hash, event, action, delivery_id, state,
			latest_output, description, started_at, finished_at, updated_at, duration_ms
		FROM projects
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []ProjectState
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		project.RecentEvents, err = s.ListProjectEvents(ctx, project.Repo, defaultProjectEventsRetain)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
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
	project.RecentEvents, err = s.ListProjectEvents(ctx, repo, defaultProjectEventsRetain)
	if err != nil {
		return nil, err
	}
	return &project, nil
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
	s.PruneProjectEvents(ctx, event.Repo, defaultProjectEventsRetain)
}

func (s *ProjectStore) ListProjectEvents(ctx context.Context, repo string, limit int) ([]ProjectEvent, error) {
	if s == nil {
		return []ProjectEvent{}, nil
	}
	if limit <= 0 {
		limit = defaultProjectEventsRetain
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
		retain = defaultProjectEventsRetain
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

func latestEventOutput(event ExecutionEvent) string {
	for i := len(event.Hooks) - 1; i >= 0; i-- {
		if event.Hooks[i].Output != "" {
			return event.Hooks[i].Output
		}
	}
	return ""
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

func logStateError(err error, msg string) {
	log.Error().Err(err).Msg(msg)
}
