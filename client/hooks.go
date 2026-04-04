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
	"gopkg.in/yaml.v3"
)

// HookSet holds all hooks resolved for a single event.
type HookSet struct {
	Pre      string // global pre hook — runs for every event
	Post     string // global post hook — runs for every event
	EventPre  string // event-specific pre hook
	EventPost string // event-specific post hook
}

// EventHooks defines pre/post hooks for a specific event or event.action.
type EventHooks struct {
	Pre  string `yaml:"pre"`
	Post string `yaml:"post"`
}

// EventicConfig is the in-repo .eventic.yaml format.
//
// Example:
//
//	hooks:
//	  pre: "echo preparing..."
//	  post: "echo done"
//	events:
//	  push:
//	    post: "bruce install .deploy/deploy.yml"
//	  pull_request:
//	    post: "make lint"
//	  pull_request.opened:
//	    post: "claude -p 'Review this PR' --headless"
//	  pull_request.synchronize:
//	    post: "make test"
//	  release.published:
//	    post: "/opt/scripts/notify-release.sh"
//	  workflow_run.completed:
//	    post: "/opt/scripts/on-build-failure.sh"
//	  issues.opened:
//	    post: "claude -p 'Triage this issue' --headless"
//	  check_suite.completed:
//	    post: "/opt/scripts/check-status.sh"
//	  deployment_status:
//	    post: "/opt/scripts/deploy-status.sh"
type EventicConfig struct {
	Hooks struct {
		Pre  string `yaml:"pre"`
		Post string `yaml:"post"`
	} `yaml:"hooks"`
	Events map[string]EventHooks `yaml:"events"`
}

// DiscoverHooks checks the repo for .eventic.yaml or .deploy/deploy.yml
// and resolves global + event-specific hooks for the given event.
// globalPre and globalPost are fallback hooks from the client config that
// are used when a repo has no hooks of its own.
func DiscoverHooks(repoPath string, event protocol.EventMsg, globalPre, globalPost string) HookSet {
	var hooks HookSet

	configPath := filepath.Join(repoPath, ".eventic.yaml")
	if data, err := os.ReadFile(configPath); err == nil {
		var cfg EventicConfig
		if err := yaml.Unmarshal(data, &cfg); err == nil {
			// Global hooks
			hooks.Pre = cfg.Hooks.Pre
			hooks.Post = cfg.Hooks.Post

			// Event-specific hooks: check "event.action" first, then "event"
			eventHooks := resolveEventHooks(cfg.Events, event.GitHubEvent, event.Action)
			hooks.EventPre = eventHooks.Pre
			hooks.EventPost = eventHooks.Post

			log.Debug().
				Str("config", configPath).
				Str("event", event.GitHubEvent).
				Str("action", event.Action).
				Bool("has_event_hook", hooks.EventPre != "" || hooks.EventPost != "").
				Msg("using .eventic.yaml")
			return hooks
		}
	}

	// Fallback: .deploy/deploy.yml as a post hook for push events
	deployPath := filepath.Join(repoPath, ".deploy", "deploy.yml")
	if _, err := os.Stat(deployPath); err == nil {
		hooks.Post = fmt.Sprintf("bruce install %s", deployPath)
		log.Debug().Str("manifest", deployPath).Msg("using bruce manifest")
		return hooks
	}

	// Fallback: client-level global hooks
	if globalPre != "" || globalPost != "" {
		hooks.Pre = globalPre
		hooks.Post = globalPost
		log.Debug().
			Str("repo", repoPath).
			Bool("has_pre", globalPre != "").
			Bool("has_post", globalPost != "").
			Msg("using client global hooks")
		return hooks
	}

	log.Debug().Str("repo", repoPath).Msg("no hooks configured")
	return hooks
}

// resolveEventHooks looks up event-specific hooks with action specificity.
// Lookup order: "event.action" (most specific) -> "event" (fallback).
func resolveEventHooks(events map[string]EventHooks, eventType, action string) EventHooks {
	if events == nil {
		return EventHooks{}
	}

	// Most specific: event.action (e.g., "pull_request.opened")
	if action != "" {
		key := eventType + "." + action
		if h, ok := events[key]; ok {
			log.Debug().Str("key", key).Msg("matched event.action hook")
			return h
		}
	}

	// Fallback: event type only (e.g., "push")
	if h, ok := events[eventType]; ok {
		log.Debug().Str("key", eventType).Msg("matched event hook")
		return h
	}

	return EventHooks{}
}

// RunHook executes a hook command in the repo directory.
func RunHook(ctx context.Context, repoPath, hook, hookLabel string, event protocol.EventMsg) error {
	_, err := RunHookWithOutput(ctx, repoPath, hook, hookLabel, event)
	return err
}

// RunHookWithOutput executes a hook and returns its combined output.
func RunHookWithOutput(ctx context.Context, repoPath, hook, hookLabel string, event protocol.EventMsg) (string, error) {
	log.Info().Msgf("Event %s on %s: running %s", event.GitHubEvent, event.Repo, hookLabel)
	log.Debug().Str("hook", hook).Str("dir", repoPath).Msg("hook command detail")

	cmd := exec.CommandContext(ctx, "sh", "-c", hook)
	cmd.Dir = repoPath
	cmd.Env = append(os.Environ(),
		"EVENTIC_REPO="+event.Repo,
		"EVENTIC_REF="+event.Ref,
		"EVENTIC_EVENT="+event.GitHubEvent,
		"EVENTIC_ACTION="+event.Action,
		"EVENTIC_SENDER="+event.Sender,
		fmt.Sprintf("EVENTIC_PR_NUMBER=%d", event.PRNumber),
		"EVENTIC_DELIVERY_ID="+event.DeliveryID,
	)

	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		log.Error().Str("output", outStr).Msgf("Event %s on %s: %s failed", event.GitHubEvent, event.Repo, hookLabel)
		return outStr, fmt.Errorf("hook failed: %w\noutput: %s", err, out)
	}

	if outStr != "" {
		log.Info().Msgf("Event %s on %s: %s output: %s", event.GitHubEvent, event.Repo, hookLabel, outStr)
	}

	return outStr, nil
}
