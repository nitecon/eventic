package client

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// maxRunNodeOutputBytes bounds the per-node output persisted to workflow_run_nodes.
const maxRunNodeOutputBytes = 65536

// Workflow scope values.
const (
	WorkflowScopeRepo   = "repo"
	WorkflowScopeGlobal = "global"
)

// Workflow is a per-(scope, repo, event_type) DAG of command nodes authored from
// the dashboard. Nodes and Edges are populated by GetWorkflow and consumed by
// CreateWorkflow/UpdateWorkflow.
type Workflow struct {
	ID        int64          `json:"id"`
	Scope     string         `json:"scope"`
	Repo      string         `json:"repo"`
	EventType string         `json:"event_type"`
	Name      string         `json:"name"`
	Enabled   bool           `json:"enabled"`
	Version   int64          `json:"version"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Nodes     []WorkflowNode `json:"nodes,omitempty"`
	Edges     []WorkflowEdge `json:"edges,omitempty"`
}

// WorkflowNode is a single command step within a workflow DAG.
type WorkflowNode struct {
	ID              int64   `json:"id"`
	NodeKey         string  `json:"node_key"`
	Name            string  `json:"name"`
	Type            string  `json:"type"`
	Command         string  `json:"command"`
	Capture         string  `json:"capture"`
	ContinueOnError bool    `json:"continue_on_error"`
	TimeoutSeconds  int64   `json:"timeout_seconds"`
	PosX            float64 `json:"pos_x"`
	PosY            float64 `json:"pos_y"`
	Config          string  `json:"config"`
}

// WorkflowEdge is a directed, optionally-conditional link between two nodes.
type WorkflowEdge struct {
	ID        int64  `json:"id"`
	FromNode  string `json:"from_node"`
	ToNode    string `json:"to_node"`
	Condition string `json:"condition"`
}

// WorkflowRun is a single execution of a workflow against an event.
type WorkflowRun struct {
	ID             int64             `json:"id"`
	WorkflowID     int64             `json:"workflow_id"`
	Repo           string            `json:"repo"`
	EventType      string            `json:"event_type"`
	DeliveryID     string            `json:"delivery_id"`
	Ref            string            `json:"ref,omitempty"`
	Hash           string            `json:"hash,omitempty"`
	Trigger        string            `json:"trigger,omitempty"`
	State          string            `json:"state"`
	StartedAt      time.Time         `json:"started_at"`
	FinishedAt     *time.Time        `json:"finished_at,omitempty"`
	DurationMillis int64             `json:"duration_ms,omitempty"`
	Nodes          []WorkflowRunNode `json:"nodes,omitempty"`
}

// WorkflowRunNode is the per-node execution record for a workflow run.
type WorkflowRunNode struct {
	ID             int64      `json:"id"`
	RunID          int64      `json:"run_id"`
	NodeKey        string     `json:"node_key"`
	State          string     `json:"state"`
	ExitCode       int64      `json:"exit_code"`
	Output         string     `json:"output,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	DurationMillis int64      `json:"duration_ms,omitempty"`
}

// ── Workflow CRUD ───────────────────────────────────────────────────────────

// ListWorkflows returns workflow summaries (without nodes/edges) filtered by the
// supplied scope, repo, and eventType. Empty filter values are ignored.
func (s *ProjectStore) ListWorkflows(ctx context.Context, scope, repo, eventType string) ([]Workflow, error) {
	if s == nil {
		return []Workflow{}, nil
	}

	query := `
		SELECT id, scope, repo, event_type, name, enabled, version, created_at, updated_at
		FROM workflows
		WHERE 1 = 1`
	args := []any{}
	if scope != "" {
		query += " AND scope = ?"
		args = append(args, scope)
	}
	if repo != "" {
		query += " AND repo = ?"
		args = append(args, repo)
	}
	if eventType != "" {
		query += " AND event_type = ?"
		args = append(args, eventType)
	}
	query += " ORDER BY scope, repo, event_type"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workflows []Workflow
	for rows.Next() {
		wf, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, wf)
	}
	return workflows, rows.Err()
}

// GetWorkflow returns a single workflow with its nodes and edges populated.
func (s *ProjectStore) GetWorkflow(ctx context.Context, id int64) (*Workflow, error) {
	if s == nil {
		return nil, sql.ErrNoRows
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT id, scope, repo, event_type, name, enabled, version, created_at, updated_at
		FROM workflows
		WHERE id = ?
	`, id)
	wf, err := scanWorkflow(row)
	if err != nil {
		return nil, err
	}

	wf.Nodes, err = s.workflowNodes(ctx, id)
	if err != nil {
		return nil, err
	}
	wf.Edges, err = s.workflowEdges(ctx, id)
	if err != nil {
		return nil, err
	}
	return &wf, nil
}

// CreateWorkflow inserts a new workflow plus its nodes and edges in a single
// transaction and returns the new workflow id.
func (s *ProjectStore) CreateWorkflow(ctx context.Context, wf *Workflow) (int64, error) {
	if s == nil || wf == nil {
		return 0, nil
	}

	now := time.Now()
	version := wf.Version
	if version <= 0 {
		version = 1
	}

	var id int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO workflows (
				scope, repo, event_type, name, enabled, version, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, wf.Scope, wf.Repo, wf.EventType, wf.Name, boolToInt(wf.Enabled), version, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		return insertWorkflowGraph(ctx, tx, id, wf.Nodes, wf.Edges)
	})
	if err != nil {
		return 0, fmt.Errorf("create workflow: %w", err)
	}
	return id, nil
}

// UpdateWorkflow updates a workflow's metadata and fully replaces its nodes and
// edges inside a transaction, bumping the workflow version.
func (s *ProjectStore) UpdateWorkflow(ctx context.Context, wf *Workflow) error {
	if s == nil || wf == nil {
		return nil
	}

	now := time.Now()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE workflows
			SET scope = ?,
				repo = ?,
				event_type = ?,
				name = ?,
				enabled = ?,
				version = version + 1,
				updated_at = ?
			WHERE id = ?
		`, wf.Scope, wf.Repo, wf.EventType, wf.Name, boolToInt(wf.Enabled), now, wf.ID)
		if err != nil {
			return fmt.Errorf("update workflow: %w", err)
		}
		if rows, _ := res.RowsAffected(); rows == 0 {
			return sql.ErrNoRows
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_nodes WHERE workflow_id = ?`, wf.ID); err != nil {
			return fmt.Errorf("clear workflow nodes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_edges WHERE workflow_id = ?`, wf.ID); err != nil {
			return fmt.Errorf("clear workflow edges: %w", err)
		}
		return insertWorkflowGraph(ctx, tx, wf.ID, wf.Nodes, wf.Edges)
	})
}

// DeleteWorkflow removes a workflow and its nodes/edges.
func (s *ProjectStore) DeleteWorkflow(ctx context.Context, id int64) error {
	if s == nil {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_nodes WHERE workflow_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_edges WHERE workflow_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, id)
		return err
	})
}

// SetWorkflowEnabled toggles a workflow's enabled flag.
func (s *ProjectStore) SetWorkflowEnabled(ctx context.Context, id int64, enabled bool) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflows
		SET enabled = ?, updated_at = ?
		WHERE id = ?
	`, boolToInt(enabled), time.Now(), id)
	return err
}

// ── Resolution ──────────────────────────────────────────────────────────────

// ResolveWorkflow selects the most specific enabled workflow for an event using
// the precedence:
//
//	(repo, event.action) → (repo, event) → (global, event.action) → (global, event)
//
// It returns (nil, nil) when no enabled workflow matches.
func (s *ProjectStore) ResolveWorkflow(ctx context.Context, repo, eventType, action string) (*Workflow, error) {
	if s == nil {
		return nil, nil
	}

	type candidate struct {
		scope     string
		repo      string
		eventType string
	}

	candidates := []candidate{}
	if action != "" {
		candidates = append(candidates, candidate{WorkflowScopeRepo, repo, eventType + "." + action})
	}
	candidates = append(candidates, candidate{WorkflowScopeRepo, repo, eventType})
	if action != "" {
		candidates = append(candidates, candidate{WorkflowScopeGlobal, "", eventType + "." + action})
	}
	candidates = append(candidates, candidate{WorkflowScopeGlobal, "", eventType})

	for _, c := range candidates {
		row := s.db.QueryRowContext(ctx, `
			SELECT id, scope, repo, event_type, name, enabled, version, created_at, updated_at
			FROM workflows
			WHERE scope = ? AND repo = ? AND event_type = ? AND enabled = 1
		`, c.scope, c.repo, c.eventType)
		wf, err := scanWorkflow(row)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		wf.Nodes, err = s.workflowNodes(ctx, wf.ID)
		if err != nil {
			return nil, err
		}
		wf.Edges, err = s.workflowEdges(ctx, wf.ID)
		if err != nil {
			return nil, err
		}
		return &wf, nil
	}
	return nil, nil
}

// ── Runs ────────────────────────────────────────────────────────────────────

// StartRun inserts a workflow_runs row in the running state and returns its id.
func (s *ProjectStore) StartRun(ctx context.Context, run *WorkflowRun) (int64, error) {
	if s == nil || run == nil {
		return 0, nil
	}
	started := run.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	state := run.State
	if state == "" {
		state = "running"
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			workflow_id, repo, event_type, delivery_id, ref, hash, trigger, state, started_at, duration_ms
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
	`, run.WorkflowID, run.Repo, run.EventType, run.DeliveryID, run.Ref, run.Hash, run.Trigger, state, started)
	if err != nil {
		return 0, fmt.Errorf("start workflow run: %w", err)
	}
	return res.LastInsertId()
}

// StartRunNode inserts (or resets) a workflow_run_nodes row in the running state.
func (s *ProjectStore) StartRunNode(ctx context.Context, runID int64, nodeKey string) {
	if s == nil {
		return
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_run_nodes (
			run_id, node_key, state, exit_code, output, started_at, duration_ms
		)
		VALUES (?, ?, 'running', 0, '', ?, 0)
	`, runID, nodeKey, now)
	if err != nil {
		logStateError(err, "start workflow run node")
	}
}

// FinishRunNode marks a run node terminal, recording its state, exit code, and
// bounded output. The node duration is derived from its recorded start time.
func (s *ProjectStore) FinishRunNode(ctx context.Context, runID int64, nodeKey, state string, exitCode int, output string) {
	if s == nil {
		return
	}
	now := time.Now()
	bounded := trimString(output, maxRunNodeOutputBytes)

	var startedAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `
		SELECT started_at FROM workflow_run_nodes WHERE run_id = ? AND node_key = ?
	`, runID, nodeKey).Scan(&startedAt); err != nil && err != sql.ErrNoRows {
		logStateError(err, "read workflow run node start time")
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_run_nodes
		SET state = ?,
			exit_code = ?,
			output = ?,
			finished_at = ?,
			duration_ms = ?
		WHERE run_id = ? AND node_key = ?
	`, state, exitCode, bounded, now, durationMillis(startedAt, now), runID, nodeKey)
	if err != nil {
		logStateError(err, "finish workflow run node")
	}
}

// FinishRun marks a run terminal, recording its final state and duration derived
// from its recorded start time.
func (s *ProjectStore) FinishRun(ctx context.Context, runID int64, state string) {
	if s == nil {
		return
	}
	now := time.Now()

	var startedAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `
		SELECT started_at FROM workflow_runs WHERE id = ?
	`, runID).Scan(&startedAt); err != nil && err != sql.ErrNoRows {
		logStateError(err, "read workflow run start time")
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_runs
		SET state = ?,
			finished_at = ?,
			duration_ms = ?
		WHERE id = ?
	`, state, now, durationMillis(startedAt, now), runID)
	if err != nil {
		logStateError(err, "finish workflow run")
	}
}

// durationMillis returns the elapsed milliseconds between a recorded start time
// and a finish time, or 0 when no start time was recorded.
func durationMillis(startedAt sql.NullTime, finished time.Time) int64 {
	if !startedAt.Valid {
		return 0
	}
	d := finished.Sub(startedAt.Time).Milliseconds()
	if d < 0 {
		return 0
	}
	return d
}

// ListRuns returns recent run summaries (without per-node detail) for a repo,
// newest first. An empty repo lists across all repos.
func (s *ProjectStore) ListRuns(ctx context.Context, repo string, limit int) ([]WorkflowRun, error) {
	if s == nil {
		return []WorkflowRun{}, nil
	}
	if limit <= 0 {
		limit = 20
	}

	query := `
		SELECT id, workflow_id, repo, event_type, delivery_id, ref, hash, trigger, state, started_at, finished_at, duration_ms
		FROM workflow_runs`
	args := []any{}
	if repo != "" {
		query += " WHERE repo = ?"
		args = append(args, repo)
	}
	query += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []WorkflowRun
	for rows.Next() {
		run, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// GetRun returns a single run with its per-node execution records.
func (s *ProjectStore) GetRun(ctx context.Context, id int64) (*WorkflowRun, error) {
	if s == nil {
		return nil, sql.ErrNoRows
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT id, workflow_id, repo, event_type, delivery_id, ref, hash, trigger, state, started_at, finished_at, duration_ms
		FROM workflow_runs
		WHERE id = ?
	`, id)
	run, err := scanWorkflowRun(row)
	if err != nil {
		return nil, err
	}

	nodes, err := s.runNodes(ctx, id)
	if err != nil {
		return nil, err
	}
	run.Nodes = nodes
	return &run, nil
}

// PruneRuns retains only the newest `retain` runs for a repo, deleting older
// runs and their node records.
func (s *ProjectStore) PruneRuns(ctx context.Context, repo string, retain int) {
	if s == nil {
		return
	}
	if retain <= 0 {
		retain = 50
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_run_nodes
		WHERE run_id IN (
			SELECT id FROM workflow_runs
			WHERE repo = ?
				AND id NOT IN (
					SELECT id FROM workflow_runs
					WHERE repo = ?
					ORDER BY started_at DESC
					LIMIT ?
				)
		)
	`, repo, repo, retain)
	if err != nil {
		logStateError(err, "prune workflow run nodes")
	}
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM workflow_runs
		WHERE repo = ?
			AND id NOT IN (
				SELECT id FROM workflow_runs
				WHERE repo = ?
				ORDER BY started_at DESC
				LIMIT ?
			)
	`, repo, repo, retain)
	if err != nil {
		logStateError(err, "prune workflow runs")
	}
}

// ── Internal helpers ────────────────────────────────────────────────────────

// withTx runs fn inside a transaction, committing on success and rolling back
// on error.
func (s *ProjectStore) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// insertWorkflowGraph inserts the nodes and edges for a workflow within an
// existing transaction.
func insertWorkflowGraph(ctx context.Context, tx *sql.Tx, workflowID int64, nodes []WorkflowNode, edges []WorkflowEdge) error {
	for _, node := range nodes {
		nodeType := normalizeActionType(node.Type)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_nodes (
				workflow_id, node_key, name, type, command, capture,
				continue_on_error, timeout_seconds, pos_x, pos_y, config
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, workflowID, node.NodeKey, node.Name, nodeType, node.Command, node.Capture,
			boolToInt(node.ContinueOnError), node.TimeoutSeconds, node.PosX, node.PosY, node.Config); err != nil {
			return fmt.Errorf("insert workflow node: %w", err)
		}
	}
	for _, edge := range edges {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_edges (workflow_id, from_node, to_node, condition)
			VALUES (?, ?, ?, ?)
		`, workflowID, edge.FromNode, edge.ToNode, edge.Condition); err != nil {
			return fmt.Errorf("insert workflow edge: %w", err)
		}
	}
	return nil
}

func (s *ProjectStore) workflowNodes(ctx context.Context, workflowID int64) ([]WorkflowNode, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_key, name, type, command, capture,
			continue_on_error, timeout_seconds, pos_x, pos_y, config
		FROM workflow_nodes
		WHERE workflow_id = ?
		ORDER BY id
	`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []WorkflowNode
	for rows.Next() {
		var node WorkflowNode
		var continueOnError int64
		if err := rows.Scan(
			&node.ID, &node.NodeKey, &node.Name, &node.Type, &node.Command, &node.Capture,
			&continueOnError, &node.TimeoutSeconds, &node.PosX, &node.PosY, &node.Config,
		); err != nil {
			return nil, err
		}
		node.ContinueOnError = continueOnError != 0
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (s *ProjectStore) workflowEdges(ctx context.Context, workflowID int64) ([]WorkflowEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, from_node, to_node, condition
		FROM workflow_edges
		WHERE workflow_id = ?
		ORDER BY id
	`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []WorkflowEdge
	for rows.Next() {
		var edge WorkflowEdge
		if err := rows.Scan(&edge.ID, &edge.FromNode, &edge.ToNode, &edge.Condition); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

func (s *ProjectStore) runNodes(ctx context.Context, runID int64) ([]WorkflowRunNode, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, node_key, state, exit_code, output, started_at, finished_at, duration_ms
		FROM workflow_run_nodes
		WHERE run_id = ?
		ORDER BY id
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []WorkflowRunNode
	for rows.Next() {
		node, err := scanWorkflowRunNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func scanWorkflow(scanner projectScanner) (Workflow, error) {
	var wf Workflow
	var enabled int64
	err := scanner.Scan(
		&wf.ID, &wf.Scope, &wf.Repo, &wf.EventType, &wf.Name,
		&enabled, &wf.Version, &wf.CreatedAt, &wf.UpdatedAt,
	)
	if err != nil {
		return Workflow{}, err
	}
	wf.Enabled = enabled != 0
	return wf, nil
}

func scanWorkflowRun(scanner projectScanner) (WorkflowRun, error) {
	var run WorkflowRun
	var finishedAt sql.NullTime
	err := scanner.Scan(
		&run.ID, &run.WorkflowID, &run.Repo, &run.EventType, &run.DeliveryID,
		&run.Ref, &run.Hash, &run.Trigger, &run.State, &run.StartedAt, &finishedAt, &run.DurationMillis,
	)
	if err != nil {
		return WorkflowRun{}, err
	}
	if finishedAt.Valid {
		run.FinishedAt = &finishedAt.Time
	}
	return run, nil
}

func scanWorkflowRunNode(scanner projectScanner) (WorkflowRunNode, error) {
	var node WorkflowRunNode
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	err := scanner.Scan(
		&node.ID, &node.RunID, &node.NodeKey, &node.State, &node.ExitCode,
		&node.Output, &startedAt, &finishedAt, &node.DurationMillis,
	)
	if err != nil {
		return WorkflowRunNode{}, err
	}
	if startedAt.Valid {
		node.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		node.FinishedAt = &finishedAt.Time
	}
	return node, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
