package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/eventic/protocol"
)

const maxInboundEventPayloadBytes = 65536

// InboundEventRecord is the persisted audit view of one inbound event after
// Eventic has applied provider normalization and stable-event mapping rules.
type InboundEventRecord struct {
	ID             int64             `json:"id"`
	DeliveryID     string            `json:"delivery_id"`
	Repo           string            `json:"repo"`
	Ref            string            `json:"ref,omitempty"`
	Provider       string            `json:"provider"`
	ExternalEvent  string            `json:"external_event,omitempty"`
	ExternalAction string            `json:"external_action,omitempty"`
	StableEvent    string            `json:"stable_event,omitempty"`
	MappingID      string            `json:"mapping_id,omitempty"`
	MappingName    string            `json:"mapping_name,omitempty"`
	MappingStatus  string            `json:"mapping_status"`
	Actor          string            `json:"actor,omitempty"`
	Severity       string            `json:"severity,omitempty"`
	Message        string            `json:"message,omitempty"`
	CloneURL       string            `json:"clone_url,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Payload        json.RawMessage   `json:"payload,omitempty"`
	State          string            `json:"state"`
	Description    string            `json:"description,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

func (s *ProjectStore) RecordInboundEvent(ctx context.Context, event protocol.EventMsg, normalized NormalizedEvent) error {
	if s == nil {
		return nil
	}
	now := time.Now()
	metadata := normalized.Metadata
	if metadata == nil {
		metadata = event.Metadata
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal inbound event metadata: %w", err)
	}
	payload := string(event.Payload)
	if len(payload) > maxInboundEventPayloadBytes {
		payload = ""
	}
	severity := ""
	if metadata != nil {
		severity = strings.ToUpper(strings.TrimSpace(metadata["severity"]))
	}
	state := "received"
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO inbound_events (
			delivery_id, repo, ref, provider, external_event, external_action, stable_event,
			mapping_id, mapping_name, mapping_status, actor, severity, message, clone_url,
			metadata, payload, state, description, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(delivery_id) DO UPDATE SET
			repo = excluded.repo,
			ref = excluded.ref,
			provider = excluded.provider,
			external_event = excluded.external_event,
			external_action = excluded.external_action,
			stable_event = excluded.stable_event,
			mapping_id = excluded.mapping_id,
			mapping_name = excluded.mapping_name,
			mapping_status = excluded.mapping_status,
			actor = excluded.actor,
			severity = excluded.severity,
			message = excluded.message,
			clone_url = excluded.clone_url,
			metadata = excluded.metadata,
			payload = excluded.payload,
			updated_at = excluded.updated_at
	`, event.DeliveryID, event.Repo, event.Ref, eventProvider(event),
		firstNonEmpty(event.ExternalEvent, event.GitHubEvent), firstNonEmpty(event.ExternalAction, event.Action),
		normalized.StableEvent, normalized.MappingID, normalized.MappingName, normalized.MappingStatus,
		event.Sender, severity, event.Message, event.CloneURL, string(metadataJSON), payload, state, now, now)
	if err != nil {
		return fmt.Errorf("record inbound event: %w", err)
	}
	return nil
}

func (s *ProjectStore) FinishInboundEvent(ctx context.Context, deliveryID, state, description string) error {
	if s == nil || strings.TrimSpace(deliveryID) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE inbound_events
		SET state = ?, description = ?, updated_at = ?
		WHERE delivery_id = ?
	`, strings.TrimSpace(state), trimString(description, maxInboundEventPayloadBytes), time.Now(), deliveryID)
	if err != nil {
		return fmt.Errorf("finish inbound event: %w", err)
	}
	return nil
}

func (s *ProjectStore) ListInboundEvents(ctx context.Context, filter InboundEventFilter) ([]InboundEventRecord, error) {
	if s == nil {
		return []InboundEventRecord{}, nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 250 {
		limit = 250
	}
	query := `
		SELECT id, delivery_id, repo, ref, provider, external_event, external_action, stable_event,
			mapping_id, mapping_name, mapping_status, actor, severity, message, clone_url,
			metadata, payload, state, description, created_at, updated_at
		FROM inbound_events
		WHERE 1 = 1`
	args := []any{}
	if filter.Repo != "" {
		query += " AND repo = ?"
		args = append(args, filter.Repo)
	}
	if filter.Provider != "" {
		query += " AND provider = ?"
		args = append(args, filter.Provider)
	}
	if filter.StableEvent != "" {
		query += " AND stable_event = ?"
		args = append(args, filter.StableEvent)
	}
	if filter.State != "" {
		query += " AND state = ?"
		args = append(args, filter.State)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []InboundEventRecord
	for rows.Next() {
		record, err := scanInboundEvent(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *ProjectStore) GetInboundEvent(ctx context.Context, id int64) (*InboundEventRecord, error) {
	if s == nil {
		return nil, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, delivery_id, repo, ref, provider, external_event, external_action, stable_event,
			mapping_id, mapping_name, mapping_status, actor, severity, message, clone_url,
			metadata, payload, state, description, created_at, updated_at
		FROM inbound_events
		WHERE id = ?
	`, id)
	record, err := scanInboundEvent(row)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

type InboundEventFilter struct {
	Repo        string
	Provider    string
	StableEvent string
	State       string
	Limit       int
}

func scanInboundEvent(scanner projectScanner) (InboundEventRecord, error) {
	var record InboundEventRecord
	var metadataText string
	var payloadText string
	err := scanner.Scan(
		&record.ID, &record.DeliveryID, &record.Repo, &record.Ref, &record.Provider,
		&record.ExternalEvent, &record.ExternalAction, &record.StableEvent,
		&record.MappingID, &record.MappingName, &record.MappingStatus,
		&record.Actor, &record.Severity, &record.Message, &record.CloneURL,
		&metadataText, &payloadText, &record.State, &record.Description,
		&record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		return InboundEventRecord{}, err
	}
	record.Metadata = map[string]string{}
	if strings.TrimSpace(metadataText) != "" {
		if err := json.Unmarshal([]byte(metadataText), &record.Metadata); err != nil {
			return InboundEventRecord{}, err
		}
	}
	if strings.TrimSpace(payloadText) != "" {
		record.Payload = json.RawMessage(payloadText)
	}
	return record, nil
}

func eventFromInboundRecord(record InboundEventRecord, deliveryID string) protocol.EventMsg {
	eventName := firstNonEmpty(record.ExternalEvent, "internal")
	action := record.ExternalAction
	if record.StableEvent != "" {
		eventName = "internal"
		action = ""
	}
	cloneURL := record.CloneURL
	if cloneURL == "" && record.Repo != "" {
		cloneURL = "https://github.com/" + record.Repo + ".git"
	}
	return protocol.EventMsg{
		MsgType:        "Event",
		DeliveryID:     deliveryID,
		GitHubEvent:    eventName,
		Provider:       firstNonEmpty(record.Provider, "eventic"),
		StableEvent:    record.StableEvent,
		ExternalEvent:  record.ExternalEvent,
		ExternalAction: record.ExternalAction,
		Repo:           record.Repo,
		Ref:            record.Ref,
		Action:         action,
		Sender:         firstNonEmpty(record.Actor, "eventic-web"),
		Message:        record.Message,
		CloneURL:       cloneURL,
		Metadata:       record.Metadata,
		Payload:        record.Payload,
	}
}
