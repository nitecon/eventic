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

func TestShouldRunGlobalHook(t *testing.T) {
	tests := []struct {
		name            string
		eventType       string
		action          string
		allowedPatterns []string
		ignorePatterns  []string
		want            bool
	}{
		// Allowed list empty — falls back to ignore logic
		{"no allowed, no ignore", "push", "", nil, nil, true},
		{"no allowed, ignore matches", "check_run", "completed", nil, []string{"check_run.completed"}, false},
		{"no allowed, ignore no match", "push", "", nil, []string{"check_run.completed"}, true},

		// Allowed list non-empty — ignore list is disregarded
		{"allowed exact match", "workflow_job", "completed", []string{"workflow_job.completed"}, nil, true},
		{"allowed no match", "push", "", []string{"workflow_job.completed"}, nil, false},
		{"allowed wildcard action", "workflow_job", "completed", []string{"workflow_job.*"}, nil, true},
		{"allowed wildcard event", "workflow_job", "completed", []string{"*.completed"}, nil, true},
		{"allowed bare event", "push", "", []string{"push"}, nil, true},
		{"allowed star matches all", "check_run", "created", []string{"*"}, nil, true},

		// Allowed overrides ignore
		{"allowed overrides ignore", "workflow_job", "completed",
			[]string{"workflow_job.completed"},
			[]string{"workflow_job.completed"}, // would be ignored, but allowed takes precedence
			true},
		{"allowed blocks what ignore would allow", "push", "",
			[]string{"workflow_job.completed"},
			[]string{}, // ignore list is empty so push would normally run
			false},

		// Multiple allowed patterns
		{"multiple allowed, second matches", "push", "",
			[]string{"workflow_job.completed", "push"}, nil, true},
		{"multiple allowed, none match", "check_run", "created",
			[]string{"workflow_job.completed", "push"}, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRunGlobalHook(tt.eventType, tt.action, tt.allowedPatterns, tt.ignorePatterns)
			if got != tt.want {
				t.Errorf("shouldRunGlobalHook(%q, %q, %v, %v) = %v, want %v",
					tt.eventType, tt.action, tt.allowedPatterns, tt.ignorePatterns, got, tt.want)
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

func TestShouldNotify(t *testing.T) {
	tests := []struct {
		name     string
		notifyOn []string
		state    string
		want     bool
	}{
		{"empty filter always notifies on success", nil, "success", true},
		{"empty filter always notifies on failure", nil, "failure", true},
		{"failure-only filter blocks success", []string{"failure"}, "success", false},
		{"failure-only filter allows failure", []string{"failure"}, "failure", true},
		{"success-only filter allows success", []string{"success"}, "success", true},
		{"success-only filter blocks failure", []string{"success"}, "failure", false},
		{"multi-state filter allows failure", []string{"success", "failure"}, "failure", true},
		{"multi-state filter allows success", []string{"success", "failure"}, "success", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldNotify(tt.notifyOn, tt.state)
			if got != tt.want {
				t.Errorf("ShouldNotify(%v, %q) = %v, want %v", tt.notifyOn, tt.state, got, tt.want)
			}
		})
	}
}
