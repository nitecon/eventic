package client

import (
	"context"
	"sync"
	"testing"
)

// scriptedRunner returns a NodeRunner that records execution order and returns
// per-node scripted results. Captured output is supplied so capture vars flow.
type scriptedRunner struct {
	mu      sync.Mutex
	order   []string
	results map[string]NodeResult // keyed by node key
	seen    map[string]RunContext // snapshot of vars observed per node
}

func newScriptedRunner(results map[string]NodeResult) *scriptedRunner {
	return &scriptedRunner{
		results: results,
		seen:    make(map[string]RunContext),
	}
}

func (s *scriptedRunner) run(_ context.Context, step DAGStep, rc *RunContext) NodeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, step.Key)
	// snapshot the vars visible to this node
	varsCopy := make(map[string]string, len(rc.Vars))
	for k, v := range rc.Vars {
		varsCopy[k] = v
	}
	s.seen[step.Key] = RunContext{Vars: varsCopy, WorkspaceDir: rc.WorkspaceDir}

	res, ok := s.results[step.Key]
	if !ok {
		res = NodeResult{State: NodeStateSuccess}
	}
	res.Key = step.Key
	return res
}

func node(key string) WorkflowNode { return WorkflowNode{NodeKey: key, Name: key} }

func graphFrom(t *testing.T, nodes []WorkflowNode, edges []WorkflowEdge) Graph {
	t.Helper()
	g, err := BuildGraph(nodes, edges)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return g
}

func resultState(results []NodeResult, key string) string {
	for _, r := range results {
		if r.Key == key {
			return r.State
		}
	}
	return "<missing>"
}

func TestExecuteTopologicalOrder(t *testing.T) {
	nodes := []WorkflowNode{node("a"), node("b"), node("c")}
	edges := []WorkflowEdge{{FromNode: "a", ToNode: "b"}, {FromNode: "b", ToNode: "c"}}
	g := graphFrom(t, nodes, edges)
	if err := Validate(g); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	runner := newScriptedRunner(nil)
	rc := &RunContext{Vars: map[string]string{}}
	results, err := Execute(context.Background(), g, rc, runner.run, 1)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	want := []string{"a", "b", "c"}
	if len(runner.order) != 3 || runner.order[0] != want[0] || runner.order[1] != want[1] || runner.order[2] != want[2] {
		t.Fatalf("execution order = %v, want %v", runner.order, want)
	}
}

func TestValidateRejectsCycle(t *testing.T) {
	nodes := []WorkflowNode{node("a"), node("b"), node("c")}
	edges := []WorkflowEdge{
		{FromNode: "a", ToNode: "b"},
		{FromNode: "b", ToNode: "c"},
		{FromNode: "c", ToNode: "a"}, // back-edge → cycle
	}
	g := graphFrom(t, nodes, edges)
	if err := Validate(g); err == nil {
		t.Fatal("expected cycle detection error, got nil")
	}
}

func TestValidateRejectsDanglingEdge(t *testing.T) {
	nodes := []WorkflowNode{node("a")}
	edges := []WorkflowEdge{{FromNode: "a", ToNode: "ghost"}}
	g := graphFrom(t, nodes, edges)
	if err := Validate(g); err == nil {
		t.Fatal("expected dangling-edge error, got nil")
	}
}

func TestExecuteSkipPropagation(t *testing.T) {
	// a (success) → b on condition "failure" (never taken) → c.
	// b is skipped because no incoming edge condition is satisfied; c is then
	// skipped because its only parent (b) was skipped.
	nodes := []WorkflowNode{node("a"), node("b"), node("c")}
	edges := []WorkflowEdge{
		{FromNode: "a", ToNode: "b", Condition: "failure"},
		{FromNode: "b", ToNode: "c"},
	}
	g := graphFrom(t, nodes, edges)

	runner := newScriptedRunner(map[string]NodeResult{
		"a": {State: NodeStateSuccess},
	})
	rc := &RunContext{Vars: map[string]string{}}
	results, err := Execute(context.Background(), g, rc, runner.run, 1)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := resultState(results, "a"); got != NodeStateSuccess {
		t.Errorf("node a state = %q, want success", got)
	}
	if got := resultState(results, "b"); got != NodeStateSkipped {
		t.Errorf("node b state = %q, want skipped", got)
	}
	if got := resultState(results, "c"); got != NodeStateSkipped {
		t.Errorf("node c state = %q, want skipped (skip propagation)", got)
	}
	for _, key := range runner.order {
		if key == "b" || key == "c" {
			t.Errorf("skipped node %q should not have run", key)
		}
	}
}

func TestEvalCondition(t *testing.T) {
	src := NodeResult{State: NodeStateSuccess}
	fail := NodeResult{State: NodeStateFailure}
	vars := map[string]string{"VERDICT": "benign", "COUNT": "3"}

	tests := []struct {
		name string
		cond string
		src  NodeResult
		want bool
	}{
		{"empty is always", "", src, true},
		{"always literal", "always", fail, true},
		{"success on success", "success", src, true},
		{"success on failure", "success", fail, false},
		{"failure on failure", "failure", fail, true},
		{"failure on success", "failure", src, false},
		{"equality benign match", "VERDICT == benign", src, true},
		{"equality risky mismatch", "VERDICT == risky", src, false},
		{"equality quoted value", `VERDICT == "benign"`, src, true},
		{"equality case-insensitive", "VERDICT == BENIGN", src, true},
		{"inequality true", "VERDICT != risky", src, true},
		{"inequality false", "VERDICT != benign", src, false},
		{"unknown var fails closed", "MISSING == x", src, false},
		{"malformed fails closed", "garbage expr", src, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalCondition(tt.cond, tt.src, vars); got != tt.want {
				t.Errorf("evalCondition(%q) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

func TestExecuteConditionalGate(t *testing.T) {
	// assess captures VERDICT; benign edge runs "act", risky edge runs "halt".
	// With VERDICT=benign, act runs and halt is skipped.
	nodes := []WorkflowNode{
		{NodeKey: "assess", Name: "assess", Capture: "VERDICT"},
		node("act"),
		node("halt"),
	}
	edges := []WorkflowEdge{
		{FromNode: "assess", ToNode: "act", Condition: "VERDICT == benign"},
		{FromNode: "assess", ToNode: "halt", Condition: "VERDICT == risky"},
	}
	g := graphFrom(t, nodes, edges)

	runner := newScriptedRunner(map[string]NodeResult{
		"assess": {State: NodeStateSuccess, Output: "benign"},
	})
	rc := &RunContext{Vars: map[string]string{}}
	results, err := Execute(context.Background(), g, rc, runner.run, 1)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := resultState(results, "act"); got != NodeStateSuccess {
		t.Errorf("node act state = %q, want success (benign branch taken)", got)
	}
	if got := resultState(results, "halt"); got != NodeStateSkipped {
		t.Errorf("node halt state = %q, want skipped (risky branch not taken)", got)
	}
	if rc.Vars["VERDICT"] != "benign" {
		t.Errorf("captured VERDICT = %q, want benign", rc.Vars["VERDICT"])
	}
}

func TestExecuteCaptureFlowsDownstream(t *testing.T) {
	// produce captures TOKEN; consume must observe TOKEN in its RunContext.Vars.
	nodes := []WorkflowNode{
		{NodeKey: "produce", Name: "produce", Capture: "TOKEN"},
		node("consume"),
	}
	edges := []WorkflowEdge{{FromNode: "produce", ToNode: "consume"}}
	g := graphFrom(t, nodes, edges)

	runner := newScriptedRunner(map[string]NodeResult{
		"produce": {State: NodeStateSuccess, Output: "  abc123  "},
	})
	rc := &RunContext{Vars: map[string]string{}}
	if _, err := Execute(context.Background(), g, rc, runner.run, 1); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	seen := runner.seen["consume"]
	if seen.Vars["TOKEN"] != "abc123" {
		t.Errorf("consume saw TOKEN = %q, want trimmed abc123", seen.Vars["TOKEN"])
	}
	if rc.Vars["TOKEN"] != "abc123" {
		t.Errorf("run context TOKEN = %q, want abc123", rc.Vars["TOKEN"])
	}
}
