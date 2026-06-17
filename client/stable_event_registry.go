package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrStableEventBuiltIn = errors.New("built-in stable events cannot be deleted or renamed")

func (s *ProjectStore) seedDefaultStableEvents(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for _, stableEvent := range StableEventDefinitions() {
		exampleSources, err := marshalStringSlice(stableEvent.ExampleSources)
		if err != nil {
			return err
		}
		workflowUses, err := marshalStringSlice(stableEvent.WorkflowUses)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO stable_events (
				event, title, event_group, description, enabled, built_in, example_sources, workflow_uses, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, stableEvent.Event, stableEvent.Title, defaultStableEventGroup(stableEvent.Group),
			stableEvent.Description, boolToInt(stableEvent.Enabled), boolToInt(stableEvent.BuiltIn),
			exampleSources, workflowUses, now, now); err != nil {
			return fmt.Errorf("seed stable event %q: %w", stableEvent.Event, err)
		}
	}
	return nil
}

func (s *ProjectStore) ListStableEvents(ctx context.Context) ([]StableEventDefinition, error) {
	if s == nil {
		return StableEventDefinitions(), nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, event, title, event_group, description, enabled, built_in, example_sources, workflow_uses, created_at, updated_at
		FROM stable_events
		ORDER BY lower(event_group), lower(event)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stableEvents []StableEventDefinition
	for rows.Next() {
		stableEvent, err := scanStableEvent(rows)
		if err != nil {
			return nil, err
		}
		stableEvents = append(stableEvents, stableEvent)
	}
	return stableEvents, rows.Err()
}

func (s *ProjectStore) GetStableEvent(ctx context.Context, id int64) (*StableEventDefinition, error) {
	if s == nil {
		return nil, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, event, title, event_group, description, enabled, built_in, example_sources, workflow_uses, created_at, updated_at
		FROM stable_events
		WHERE id = ?
	`, id)
	stableEvent, err := scanStableEvent(row)
	if err != nil {
		return nil, err
	}
	return &stableEvent, nil
}

func (s *ProjectStore) CreateStableEvent(ctx context.Context, stableEvent *StableEventDefinition) (int64, error) {
	if s == nil || stableEvent == nil {
		return 0, nil
	}
	now := time.Now()
	exampleSources, err := marshalStringSlice(stableEvent.ExampleSources)
	if err != nil {
		return 0, err
	}
	workflowUses, err := marshalStringSlice(stableEvent.WorkflowUses)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO stable_events (
			event, title, event_group, description, enabled, built_in, example_sources, workflow_uses, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?)
	`, stableEvent.Event, stableEvent.Title, defaultStableEventGroup(stableEvent.Group),
		stableEvent.Description, boolToInt(stableEvent.Enabled), exampleSources, workflowUses, now, now)
	if err != nil {
		return 0, fmt.Errorf("create stable event: %w", err)
	}
	return res.LastInsertId()
}

func (s *ProjectStore) UpdateStableEvent(ctx context.Context, stableEvent *StableEventDefinition) error {
	if s == nil || stableEvent == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var previousEvent string
	var builtIn int64
	if err := tx.QueryRowContext(ctx, `SELECT event, built_in FROM stable_events WHERE id = ?`, stableEvent.ID).Scan(&previousEvent, &builtIn); err != nil {
		return err
	}
	if builtIn != 0 && previousEvent != stableEvent.Event {
		return ErrStableEventBuiltIn
	}

	exampleSources, err := marshalStringSlice(stableEvent.ExampleSources)
	if err != nil {
		return err
	}
	workflowUses, err := marshalStringSlice(stableEvent.WorkflowUses)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE stable_events
		SET event = ?,
			title = ?,
			event_group = ?,
			description = ?,
			enabled = ?,
			example_sources = ?,
			workflow_uses = ?,
			updated_at = ?
		WHERE id = ?
	`, stableEvent.Event, stableEvent.Title, defaultStableEventGroup(stableEvent.Group),
		stableEvent.Description, boolToInt(stableEvent.Enabled), exampleSources, workflowUses,
		time.Now(), stableEvent.ID)
	if err != nil {
		return fmt.Errorf("update stable event: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return sql.ErrNoRows
	}
	if previousEvent != stableEvent.Event {
		if _, err := tx.ExecContext(ctx, `
			UPDATE event_mappings
			SET target_stable_event = ?,
				updated_at = ?
			WHERE target_stable_event = ?
		`, stableEvent.Event, time.Now(), previousEvent); err != nil {
			return fmt.Errorf("update stable event mappings: %w", err)
		}
	}
	return tx.Commit()
}

func (s *ProjectStore) DeleteStableEvent(ctx context.Context, id int64) error {
	if s == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var event string
	var builtIn int64
	if err := tx.QueryRowContext(ctx, `SELECT event, built_in FROM stable_events WHERE id = ?`, id).Scan(&event, &builtIn); err != nil {
		return err
	}
	if builtIn != 0 {
		return ErrStableEventBuiltIn
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_mappings WHERE target_stable_event = ?`, event); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM stable_events WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func scanStableEvent(scanner projectScanner) (StableEventDefinition, error) {
	var stableEvent StableEventDefinition
	var enabled int64
	var builtIn int64
	var exampleSources string
	var workflowUses string
	err := scanner.Scan(
		&stableEvent.ID,
		&stableEvent.Event,
		&stableEvent.Title,
		&stableEvent.Group,
		&stableEvent.Description,
		&enabled,
		&builtIn,
		&exampleSources,
		&workflowUses,
		&stableEvent.CreatedAt,
		&stableEvent.UpdatedAt,
	)
	if err != nil {
		return StableEventDefinition{}, err
	}
	stableEvent.Enabled = enabled != 0
	stableEvent.BuiltIn = builtIn != 0
	stableEvent.ExampleSources = unmarshalStringSlice(exampleSources)
	stableEvent.WorkflowUses = unmarshalStringSlice(workflowUses)
	return stableEvent, nil
}

func marshalStringSlice(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalStringSlice(raw string) []string {
	var values []string
	if strings.TrimSpace(raw) == "" {
		return values
	}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return []string{}
	}
	return values
}

func defaultStableEventGroup(group string) string {
	group = strings.TrimSpace(group)
	if group == "" {
		return "Custom"
	}
	return group
}
