package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

// ShouldNotify checks whether a notification should fire based on the
// notify_on filter and the current state. An empty filter means always notify.
func ShouldNotify(notifyOn []string, state string) bool {
	if len(notifyOn) == 0 {
		return true
	}
	for _, s := range notifyOn {
		if s == state {
			return true
		}
	}
	return false
}

// matchesIgnorePattern checks whether a GitHub event (type + action) matches
// a single ignore pattern. Supported patterns:
//
//	"check_run.completed"  — exact event type and action
//	"check_run.*"          — any action under check_run
//	"check_run"            — equivalent to "check_run.*" (all actions)
//	"*" or "*.*"           — matches everything
func matchesIgnorePattern(eventType, action, pattern string) bool {
	if pattern == "*" {
		return true
	}

	parts := strings.SplitN(pattern, ".", 2)
	patEvent := parts[0]

	// No dot in pattern — match event type only (all actions).
	if len(parts) == 1 {
		return patEvent == "*" || eventType == patEvent
	}

	patAction := parts[1]
	eventMatch := patEvent == "*" || eventType == patEvent
	actionMatch := patAction == "*" || action == patAction
	return eventMatch && actionMatch
}

// shouldIgnoreGlobalHook returns true if the event matches any pattern in the
// ignore list, meaning the corresponding global hook should be skipped.
func shouldIgnoreGlobalHook(eventType, action string, patterns []string) bool {
	for _, p := range patterns {
		if matchesIgnorePattern(eventType, action, p) {
			return true
		}
	}
	return false
}

// shouldRunGlobalHook decides whether a global hook should execute.
// If allowedPatterns is non-empty it acts as an allowlist — only events matching
// at least one allowed pattern will run (ignore patterns are disregarded).
// Otherwise the existing ignore-list logic applies.
func shouldRunGlobalHook(eventType, action string, allowedPatterns, ignorePatterns []string) bool {
	if len(allowedPatterns) > 0 {
		for _, p := range allowedPatterns {
			if matchesIgnorePattern(eventType, action, p) {
				return true
			}
		}
		return false
	}
	return !shouldIgnoreGlobalHook(eventType, action, ignorePatterns)
}

// eventLabel returns "event.action" when an action is present, otherwise just "event".
func eventLabel(event protocol.EventMsg) string {
	if event.Action != "" {
		return event.GitHubEvent + "." + event.Action
	}
	return event.GitHubEvent
}

// buildHookEnv assembles the environment passed to every command executed for an
// event. It starts from the process environment, layers the standard EVENTIC_*
// event variables, then appends any extra "NAME=value" entries (e.g. captured
// DAG variables or the per-run context dir). It is shared by RunHookWithOutput
// and the DAG node runner so both expose an identical contract.
func buildHookEnv(repoPath string, event protocol.EventMsg, extra ...string) []string {
	env := append(os.Environ(),
		"EVENTIC_REPO="+event.Repo,
		"EVENTIC_REPOS="+reposRootForHook(repoPath, event.Repo),
		"EVENTIC_REF="+event.Ref,
		"EVENTIC_EVENT="+event.GitHubEvent,
		"EVENTIC_ACTION="+event.Action,
		"EVENTIC_SENDER="+event.Sender,
		"EVENTIC_MESSAGE="+event.Message,
		fmt.Sprintf("EVENTIC_PR_NUMBER=%d", event.PRNumber),
		"EVENTIC_DELIVERY_ID="+event.DeliveryID,
	)
	return append(env, extra...)
}

// RunHookWithOutput executes a single shell command in the repo directory with
// the standard EVENTIC_* environment and returns its trimmed combined output.
func RunHookWithOutput(ctx context.Context, repoPath, hook, hookLabel string, event protocol.EventMsg) (string, error) {
	log.Info().Msgf("Event %s on %s: running %s", eventLabel(event), event.Repo, hookLabel)
	log.Debug().Str("hook", hook).Str("dir", repoPath).Msg("hook command detail")

	cmd := exec.CommandContext(ctx, "sh", "-c", hook)
	cmd.Dir = repoPath
	cmd.Env = buildHookEnv(repoPath, event)

	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		log.Error().Str("output", outStr).Msgf("Event %s on %s: %s failed", eventLabel(event), event.Repo, hookLabel)
		return outStr, fmt.Errorf("hook failed: %w\noutput: %s", err, out)
	}

	if outStr != "" {
		log.Info().Msgf("Event %s on %s: %s output: %s", eventLabel(event), event.Repo, hookLabel, outStr)
	}

	return outStr, nil
}

func reposRootForHook(repoPath, repo string) string {
	root := repoPath
	for range strings.Split(repo, "/") {
		root = filepath.Dir(root)
	}
	return root
}
