package client

import "testing"

func TestMatchesIgnorePattern(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		action    string
		pattern   string
		want      bool
	}{
		// Exact matches
		{"exact match", "check_run", "completed", "check_run.completed", true},
		{"exact mismatch action", "check_run", "requested", "check_run.completed", false},
		{"exact mismatch event", "check_suite", "completed", "check_run.completed", false},

		// Wildcard action
		{"wildcard action match", "check_run", "completed", "check_run.*", true},
		{"wildcard action different action", "check_run", "created", "check_run.*", true},
		{"wildcard action wrong event", "push", "", "check_run.*", false},

		// Bare event type (equivalent to event.*)
		{"bare event type with action", "check_run", "completed", "check_run", true},
		{"bare event type no action", "push", "", "push", true},
		{"bare event type mismatch", "pull_request", "opened", "push", false},

		// Global wildcards
		{"star matches all", "check_run", "completed", "*", true},
		{"star matches no action", "push", "", "*", true},
		{"star dot star", "check_run", "completed", "*.*", true},
		{"star dot star no action", "push", "", "*.*", true},

		// Wildcard event type
		{"wildcard event exact action", "check_run", "completed", "*.completed", true},
		{"wildcard event wrong action", "check_run", "created", "*.completed", false},

		// Edge cases
		{"empty pattern", "push", "", "", false},
		{"empty event type", "", "completed", "check_run.completed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesIgnorePattern(tt.eventType, tt.action, tt.pattern)
			if got != tt.want {
				t.Errorf("matchesIgnorePattern(%q, %q, %q) = %v, want %v",
					tt.eventType, tt.action, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestShouldIgnoreGlobalHook(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		action    string
		patterns  []string
		want      bool
	}{
		{"empty list", "check_run", "completed", nil, false},
		{"no match", "push", "", []string{"check_run.completed", "workflow_run.*"}, false},
		{"exact match in list", "check_run", "completed", []string{"push", "check_run.completed"}, true},
		{"wildcard match in list", "workflow_run", "in_progress", []string{"check_run", "workflow_run.*"}, true},
		{"bare event match", "deployment_status", "created", []string{"deployment_status"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldIgnoreGlobalHook(tt.eventType, tt.action, tt.patterns)
			if got != tt.want {
				t.Errorf("shouldIgnoreGlobalHook(%q, %q, %v) = %v, want %v",
					tt.eventType, tt.action, tt.patterns, got, tt.want)
			}
		})
	}
}
