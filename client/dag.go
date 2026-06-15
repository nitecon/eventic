package client

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Node and run state values used throughout the DAG engine.
const (
	NodeStateSuccess = "success"
	NodeStateFailure = "failure"
	NodeStateSkipped = "skipped"
)

// DAGStep is a single executable node in a workflow graph. It is intentionally
// free of persistence and execution concerns so the engine stays unit-testable.
type DAGStep struct {
	Key             string        // unique node key within the graph
	Name            string        // human-readable label
	Command         string        // shell command executed by the NodeRunner
	Capture         string        // when non-empty, the var name to store trimmed stdout under
	ContinueOnError bool          // a failure here does not fail the overall run
	Timeout         time.Duration // per-node timeout; 0 means no timeout
}

// DAGEdge is a directed, optionally-conditional link from one node to another.
type DAGEdge struct {
	From      string // source node key
	To        string // destination node key
	Condition string // "", "always", "success", "failure", or "NAME == value" / "NAME != value"
}

// Graph is a resolved workflow ready for validation and execution. Order
// preserves the authored node sequence so deterministic (single-worker)
// execution mirrors author intent.
type Graph struct {
	Nodes map[string]DAGStep
	Order []string
	Edges []DAGEdge
}

// NodeResult records the terminal outcome of a single node. State is one of
// NodeStateSuccess, NodeStateFailure, or NodeStateSkipped.
type NodeResult struct {
	Key      string
	State    string
	ExitCode int
	Output   string
}

// RunContext is the shared, mutable execution context handed to every node.
// Vars accumulates captured outputs (and seeds condition evaluation);
// WorkspaceDir is a per-run scratch directory; BaseEnv is the inherited
// process environment the NodeRunner extends.
type RunContext struct {
	Vars         map[string]string
	WorkspaceDir string
	BaseEnv      []string
}

// NodeRunner executes a single step and returns its result. It is injected into
// Execute so the scheduler has no direct dependency on os/exec or the DB.
type NodeRunner func(ctx context.Context, step DAGStep, rc *RunContext) NodeResult

// BuildGraph converts persisted workflow nodes and edges into an executable
// Graph. Node order follows the authored node slice order.
func BuildGraph(nodes []WorkflowNode, edges []WorkflowEdge) (Graph, error) {
	g := Graph{
		Nodes: make(map[string]DAGStep, len(nodes)),
		Order: make([]string, 0, len(nodes)),
		Edges: make([]DAGEdge, 0, len(edges)),
	}
	for _, n := range nodes {
		if n.NodeKey == "" {
			return Graph{}, fmt.Errorf("workflow node has empty node_key")
		}
		if _, dup := g.Nodes[n.NodeKey]; dup {
			return Graph{}, fmt.Errorf("duplicate node_key %q", n.NodeKey)
		}
		g.Nodes[n.NodeKey] = DAGStep{
			Key:             n.NodeKey,
			Name:            n.Name,
			Command:         n.Command,
			Capture:         n.Capture,
			ContinueOnError: n.ContinueOnError,
			Timeout:         time.Duration(n.TimeoutSeconds) * time.Second,
		}
		g.Order = append(g.Order, n.NodeKey)
	}
	for _, e := range edges {
		g.Edges = append(g.Edges, DAGEdge{From: e.FromNode, To: e.ToNode, Condition: e.Condition})
	}
	return g, nil
}

// Validate checks referential integrity (every edge endpoint references a known
// node) and rejects cycles via a Kahn topological sort. It returns a descriptive
// error on the first dangling edge or detected cycle.
func Validate(g Graph) error {
	indegree := make(map[string]int, len(g.Nodes))
	for key := range g.Nodes {
		indegree[key] = 0
	}
	adj := make(map[string][]string, len(g.Nodes))
	for _, e := range g.Edges {
		if _, ok := g.Nodes[e.From]; !ok {
			return fmt.Errorf("edge references unknown source node %q", e.From)
		}
		if _, ok := g.Nodes[e.To]; !ok {
			return fmt.Errorf("edge references unknown target node %q", e.To)
		}
		adj[e.From] = append(adj[e.From], e.To)
		indegree[e.To]++
	}

	// Kahn's algorithm: repeatedly remove zero-indegree nodes.
	queue := make([]string, 0, len(g.Nodes))
	for _, key := range g.Order {
		if indegree[key] == 0 {
			queue = append(queue, key)
		}
	}
	visited := 0
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range adj[key] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(g.Nodes) {
		return fmt.Errorf("workflow graph contains a cycle")
	}
	return nil
}

// parentsOf returns the set of node keys with an edge into the given node.
func (g Graph) parentsOf(key string) map[string]struct{} {
	parents := make(map[string]struct{})
	for _, e := range g.Edges {
		if e.To == key {
			parents[e.From] = struct{}{}
		}
	}
	return parents
}

// evalCondition decides whether an edge's condition is satisfied given the
// source node's result and the accumulated run variables.
//
// Supported forms:
//   - "" / "always"       → always true
//   - "success"           → src.State == success
//   - "failure"           → src.State == failure
//   - "NAME == value"     → vars[NAME] equals value (case-insensitive)
//   - "NAME != value"     → vars[NAME] differs from value (case-insensitive)
//
// The equality form powers the agentic gate, e.g. `VERDICT == benign`.
func evalCondition(cond string, src NodeResult, vars map[string]string) bool {
	cond = strings.TrimSpace(cond)
	switch cond {
	case "", "always":
		return true
	case NodeStateSuccess:
		return src.State == NodeStateSuccess
	case NodeStateFailure:
		return src.State == NodeStateFailure
	}

	if name, want, neg, ok := parseEquality(cond); ok {
		got := strings.TrimSpace(vars[name])
		equal := strings.EqualFold(got, want)
		if neg {
			return !equal
		}
		return equal
	}

	// Unrecognized condition: fail closed so a malformed gate never silently
	// opens a downstream branch.
	return false
}

// parseEquality splits a "NAME == value" / "NAME != value" expression into its
// parts. It reports neg=true for "!=" and strips surrounding quotes from value.
func parseEquality(expr string) (name, value string, neg, ok bool) {
	if strings.Contains(expr, "!=") {
		parts := strings.SplitN(expr, "!=", 2)
		if len(parts) != 2 {
			return "", "", false, false
		}
		return strings.TrimSpace(parts[0]), stripQuotes(strings.TrimSpace(parts[1])), true, true
	}
	if strings.Contains(expr, "==") {
		parts := strings.SplitN(expr, "==", 2)
		if len(parts) != 2 {
			return "", "", false, false
		}
		return strings.TrimSpace(parts[0]), stripQuotes(strings.TrimSpace(parts[1])), false, true
	}
	return "", "", false, false
}

// stripQuotes removes a single pair of matching surrounding single or double
// quotes from a value.
func stripQuotes(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Execute runs the graph honoring DAG dependencies, conditional edges, skip
// propagation, and a per-run concurrency bound (maxNodeWorkers). It returns the
// results for every node in authored order (including skipped nodes).
//
// Scheduling rules:
//   - A node is ELIGIBLE once all parents have reached a terminal state.
//   - A node RUNS if it is a root (no parents) OR at least one incoming edge
//     whose source actually ran (not skipped) has a satisfied condition.
//     Otherwise the node is SKIPPED. Skips propagate downstream.
//   - Eligible, runnable nodes execute concurrently under a semaphore.
func Execute(ctx context.Context, g Graph, rc *RunContext, run NodeRunner, maxNodeWorkers int) ([]NodeResult, error) {
	if maxNodeWorkers <= 0 {
		maxNodeWorkers = 1
	}
	if rc.Vars == nil {
		rc.Vars = make(map[string]string)
	}

	parents := make(map[string]map[string]struct{}, len(g.Nodes))
	for key := range g.Nodes {
		parents[key] = g.parentsOf(key)
	}

	var (
		mu        sync.Mutex
		results   = make(map[string]NodeResult, len(g.Nodes))
		completed = make(map[string]struct{}, len(g.Nodes))
		sem       = make(chan struct{}, maxNodeWorkers)
		wg        sync.WaitGroup
	)

	// shouldRun reports whether a node with at least one parent should execute,
	// based on its terminal parents and their edge conditions. Caller holds mu.
	shouldRun := func(key string) bool {
		for _, e := range g.Edges {
			if e.To != key {
				continue
			}
			src, ok := results[e.From]
			if !ok || src.State == NodeStateSkipped {
				continue // a skipped/absent parent satisfies no edge
			}
			if evalCondition(e.Condition, src, rc.Vars) {
				return true
			}
		}
		return false
	}

	// runStep executes a single node and records its result. When the node
	// captures output, the var is stored under mu so downstream conditions and
	// runners observe it.
	runStep := func(key string) {
		defer wg.Done()
		defer func() { <-sem }()

		step := g.Nodes[key]
		res := run(ctx, step, rc)
		res.Key = key

		mu.Lock()
		if step.Capture != "" && res.State == NodeStateSuccess {
			rc.Vars[step.Capture] = strings.TrimSpace(res.Output)
		}
		results[key] = res
		completed[key] = struct{}{}
		mu.Unlock()
	}

	// markSkipped records a node as skipped without running it. Caller holds mu.
	markSkipped := func(key string) {
		results[key] = NodeResult{Key: key, State: NodeStateSkipped}
		completed[key] = struct{}{}
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return orderedResults(g, results), ctx.Err()
		default:
		}

		mu.Lock()
		if len(completed) == len(g.Nodes) {
			mu.Unlock()
			break
		}

		var ready []string
		for _, key := range g.Order {
			if _, done := completed[key]; done {
				continue
			}
			// Eligible only when every parent is terminal.
			eligible := true
			for parent := range parents[key] {
				if _, ok := completed[parent]; !ok {
					eligible = false
					break
				}
			}
			if !eligible {
				continue
			}
			if len(parents[key]) == 0 || shouldRun(key) {
				ready = append(ready, key)
			} else {
				markSkipped(key)
			}
		}

		if len(ready) == 0 {
			// No runnable nodes this pass. If work is still in flight, wait for
			// it; otherwise the remaining nodes were resolved as skips above and
			// the loop will terminate on the completion check.
			inFlight := len(g.Nodes) - len(completed)
			mu.Unlock()
			if inFlight > 0 {
				wg.Wait()
			}
			continue
		}
		mu.Unlock()

		for _, key := range ready {
			select {
			case <-ctx.Done():
				wg.Wait()
				return orderedResults(g, results), ctx.Err()
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go runStep(key)
		}
		wg.Wait()
	}

	wg.Wait()
	return orderedResults(g, results), nil
}

// orderedResults returns node results in authored order, including any node that
// never produced a result (defensively recorded as skipped).
func orderedResults(g Graph, results map[string]NodeResult) []NodeResult {
	out := make([]NodeResult, 0, len(g.Order))
	for _, key := range g.Order {
		if res, ok := results[key]; ok {
			out = append(out, res)
		} else {
			out = append(out, NodeResult{Key: key, State: NodeStateSkipped})
		}
	}
	return out
}
