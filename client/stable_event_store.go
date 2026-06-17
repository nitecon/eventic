package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// EventMapping maps provider-specific webhook fields to one stable internal
// event key used by workflow resolution.
type EventMapping struct {
	ID                int64             `json:"id"`
	MappingID         string            `json:"mapping_id"`
	Provider          string            `json:"provider"`
	Name              string            `json:"name"`
	Enabled           bool              `json:"enabled"`
	Priority          int64             `json:"priority"`
	Conditions        map[string]string `json:"conditions"`
	TargetStableEvent string            `json:"target_stable_event"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

var ErrEventMappingDuplicate = errors.New("event mapping already exists")

func defaultEventMappings() []EventMapping {
	return []EventMapping{
		{
			MappingID:         "map_github_push_main_to_artifact_initiate",
			Provider:          "github",
			Name:              "GitHub main push starts artifact build",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "push", "ref": "refs/heads/main"},
			TargetStableEvent: StableEventArtifactInitiate,
		},
		{
			MappingID:         "map_github_release_published_to_artifact_published",
			Provider:          "github",
			Name:              "GitHub published release promotes artifact",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "release", "action": "published"},
			TargetStableEvent: StableEventArtifactPublished,
		},
		{
			MappingID:         "map_github_workflow_failure_to_system_failure",
			Provider:          "github",
			Name:              "GitHub workflow failure alarms project",
			Enabled:           true,
			Priority:          90,
			Conditions:        map[string]string{"event": "workflow_run", "action": "completed", "body.workflow_run.conclusion": "failure"},
			TargetStableEvent: StableEventSystemFailure,
		},
		{
			MappingID:         "map_github_dependabot_created_to_security_alarm",
			Provider:          "github",
			Name:              "GitHub Dependabot alert opens security alarm",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "dependabot_alert", "action": "created"},
			TargetStableEvent: StableEventSecurityAlarm,
		},
		{
			MappingID:         "map_github_code_scanning_to_security_alarm",
			Provider:          "github",
			Name:              "GitHub code scanning alert opens security alarm",
			Enabled:           true,
			Priority:          95,
			Conditions:        map[string]string{"event": "code_scanning_alert", "action": "created,reopened,appeared_in_branch"},
			TargetStableEvent: StableEventSecurityAlarm,
		},
		{
			MappingID:         "map_github_security_advisory_to_security_alarm",
			Provider:          "github",
			Name:              "GitHub security advisory opens security alarm",
			Enabled:           true,
			Priority:          95,
			Conditions:        map[string]string{"event": "security_advisory", "action": "published,updated"},
			TargetStableEvent: StableEventSecurityAlarm,
		},
		{
			MappingID:         "map_gitlab_push_main_to_artifact_initiate",
			Provider:          "gitlab",
			Name:              "GitLab main push starts artifact build",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "push", "ref": "refs/heads/main"},
			TargetStableEvent: StableEventArtifactInitiate,
		},
		{
			MappingID:         "map_gitlab_tag_push_to_artifact_published",
			Provider:          "gitlab",
			Name:              "GitLab tag push promotes artifact",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "tag_push", "ref": "refs/tags/*"},
			TargetStableEvent: StableEventArtifactPublished,
		},
		{
			MappingID:         "map_gitlab_merge_to_artifact_initiate",
			Provider:          "gitlab",
			Name:              "GitLab merged merge request starts artifact build",
			Enabled:           true,
			Priority:          95,
			Conditions:        map[string]string{"event": "merge_request", "action": "merge,merged"},
			TargetStableEvent: StableEventArtifactInitiate,
		},
		{
			MappingID:         "map_gitlab_pipeline_failed_to_system_failure",
			Provider:          "gitlab",
			Name:              "GitLab pipeline failure alarms project",
			Enabled:           true,
			Priority:          90,
			Conditions:        map[string]string{"event": "pipeline", "action": "failed"},
			TargetStableEvent: StableEventSystemFailure,
		},
		{
			MappingID:         "map_gitlab_vulnerability_to_security_alarm",
			Provider:          "gitlab",
			Name:              "GitLab critical vulnerability opens security alarm",
			Enabled:           true,
			Priority:          90,
			Conditions:        map[string]string{"event": "vulnerability", "metadata.severity": "CRITICAL,HIGH"},
			TargetStableEvent: StableEventSecurityAlarm,
		},
		{
			MappingID:         "map_bitbucket_push_main_to_artifact_initiate",
			Provider:          "bitbucket",
			Name:              "Bitbucket main push starts artifact build",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "repo.push", "ref": "refs/heads/main"},
			TargetStableEvent: StableEventArtifactInitiate,
		},
		{
			MappingID:         "map_bitbucket_tag_push_to_artifact_published",
			Provider:          "bitbucket",
			Name:              "Bitbucket tag push promotes artifact",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "repo.push", "ref": "refs/tags/*"},
			TargetStableEvent: StableEventArtifactPublished,
		},
		{
			MappingID:         "map_bitbucket_status_failed_to_system_failure",
			Provider:          "bitbucket",
			Name:              "Bitbucket failed status alarms project",
			Enabled:           true,
			Priority:          90,
			Conditions:        map[string]string{"event": "repo.commit_status_updated", "action": "FAILED,failed"},
			TargetStableEvent: StableEventSystemFailure,
		},
		{
			MappingID:         "map_prometheus_firing_to_system_failure",
			Provider:          "*",
			Name:              "Firing alert alarms project",
			Enabled:           true,
			Priority:          100,
			Conditions:        map[string]string{"event": "alert", "body.status": "firing"},
			TargetStableEvent: StableEventSystemFailure,
		},
		{
			MappingID:         "map_security_scan_critical_to_security_alarm",
			Provider:          "*",
			Name:              "Critical scan result opens security alarm",
			Enabled:           true,
			Priority:          80,
			Conditions:        map[string]string{"body.vulnerabilities[].severity": "CRITICAL"},
			TargetStableEvent: StableEventSecurityAlarm,
		},
		{
			MappingID:         "map_discord_command_to_communication_received",
			Provider:          "discord",
			Name:              "Discord command received",
			Enabled:           true,
			Priority:          60,
			Conditions:        map[string]string{"event": "application_command"},
			TargetStableEvent: StableEventCommunicationReceived,
		},
		{
			MappingID:         "map_comms_to_communication_received",
			Provider:          "custom",
			Name:              "Internal comms message received",
			Enabled:           true,
			Priority:          50,
			Conditions:        map[string]string{"event": "comms"},
			TargetStableEvent: StableEventCommunicationReceived,
		},
	}
}

func (s *ProjectStore) seedDefaultEventMappings(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for _, mapping := range defaultEventMappings() {
		conditions, err := marshalEventMappingConditions(mapping.Conditions)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO event_mappings (
				mapping_id, provider, name, enabled, priority, conditions, target_stable_event, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, mapping.MappingID, mapping.Provider, mapping.Name, boolToInt(mapping.Enabled), mapping.Priority,
			conditions, mapping.TargetStableEvent, now, now); err != nil {
			return fmt.Errorf("seed event mapping %q: %w", mapping.MappingID, err)
		}
	}
	return nil
}

func (s *ProjectStore) ListEventMappings(ctx context.Context) ([]EventMapping, error) {
	if s == nil {
		return []EventMapping{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, mapping_id, provider, name, enabled, priority, conditions, target_stable_event, created_at, updated_at
		FROM event_mappings
		ORDER BY priority DESC, mapping_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []EventMapping
	for rows.Next() {
		mapping, err := scanEventMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	sortEventMappings(mappings)
	return mappings, rows.Err()
}

func (s *ProjectStore) GetEventMapping(ctx context.Context, id int64) (EventMapping, error) {
	if s == nil {
		return EventMapping{}, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, mapping_id, provider, name, enabled, priority, conditions, target_stable_event, created_at, updated_at
		FROM event_mappings
		WHERE id = ?
	`, id)
	return scanEventMapping(row)
}

func (s *ProjectStore) CreateEventMapping(ctx context.Context, mapping *EventMapping) (int64, error) {
	if s == nil || mapping == nil {
		return 0, nil
	}
	if duplicate, err := s.hasDuplicateEventMapping(ctx, mapping, 0); err != nil {
		return 0, err
	} else if duplicate {
		return 0, ErrEventMappingDuplicate
	}
	now := time.Now()
	conditions, err := marshalEventMappingConditions(mapping.Conditions)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO event_mappings (
			mapping_id, provider, name, enabled, priority, conditions, target_stable_event, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, mapping.MappingID, mapping.Provider, mapping.Name, boolToInt(mapping.Enabled), mapping.Priority,
		conditions, mapping.TargetStableEvent, now, now)
	if err != nil {
		return 0, fmt.Errorf("create event mapping: %w", err)
	}
	return res.LastInsertId()
}

func (s *ProjectStore) UpdateEventMapping(ctx context.Context, mapping *EventMapping) error {
	if s == nil || mapping == nil {
		return nil
	}
	if duplicate, err := s.hasDuplicateEventMapping(ctx, mapping, mapping.ID); err != nil {
		return err
	} else if duplicate {
		return ErrEventMappingDuplicate
	}
	conditions, err := marshalEventMappingConditions(mapping.Conditions)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE event_mappings
		SET mapping_id = ?,
			provider = ?,
			name = ?,
			enabled = ?,
			priority = ?,
			conditions = ?,
			target_stable_event = ?,
			updated_at = ?
		WHERE id = ?
	`, mapping.MappingID, mapping.Provider, mapping.Name, boolToInt(mapping.Enabled), mapping.Priority,
		conditions, mapping.TargetStableEvent, time.Now(), mapping.ID)
	if err != nil {
		return fmt.Errorf("update event mapping: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *ProjectStore) hasDuplicateEventMapping(ctx context.Context, mapping *EventMapping, excludeID int64) (bool, error) {
	if s == nil || mapping == nil {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conditions
		FROM event_mappings
		WHERE provider = ? AND target_stable_event = ? AND id != ?
	`, mapping.Provider, mapping.TargetStableEvent, excludeID)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var conditionsText string
		if err := rows.Scan(&id, &conditionsText); err != nil {
			return false, err
		}
		var existing map[string]string
		if strings.TrimSpace(conditionsText) == "" {
			existing = map[string]string{}
		} else if err := json.Unmarshal([]byte(conditionsText), &existing); err != nil {
			return false, err
		}
		if reflect.DeepEqual(existing, mapping.Conditions) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *ProjectStore) DeleteEventMapping(ctx context.Context, id int64) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM event_mappings WHERE id = ?`, id)
	return err
}

func scanEventMapping(scanner projectScanner) (EventMapping, error) {
	var mapping EventMapping
	var enabled int64
	var conditions string
	err := scanner.Scan(
		&mapping.ID, &mapping.MappingID, &mapping.Provider, &mapping.Name,
		&enabled, &mapping.Priority, &conditions, &mapping.TargetStableEvent,
		&mapping.CreatedAt, &mapping.UpdatedAt,
	)
	if err != nil {
		return EventMapping{}, err
	}
	mapping.Enabled = enabled != 0
	mapping.Conditions = map[string]string{}
	if conditions != "" {
		if err := json.Unmarshal([]byte(conditions), &mapping.Conditions); err != nil {
			return EventMapping{}, err
		}
	}
	return mapping, nil
}

func marshalEventMappingConditions(conditions map[string]string) (string, error) {
	if conditions == nil {
		conditions = map[string]string{}
	}
	data, err := json.Marshal(conditions)
	if err != nil {
		return "", fmt.Errorf("marshal event mapping conditions: %w", err)
	}
	return string(data), nil
}
