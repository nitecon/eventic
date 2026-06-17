package client

import (
	"context"
	"errors"
	"os/exec"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

// maxNodeOutputBytes bounds the combined output captured per node before it is
// stored or streamed. It mirrors the persistence bound in workflow_store.go.
const maxNodeOutputBytes = 65536

// newNodeRunner returns a NodeRunner that executes each typed workflow action.
// Run-command actions execute via `sh -c` in the checked-out repo directory. The
// command environment combines the shared EVENTIC_* variables (buildHookEnv),
// the per-run context dir (EVENTIC_CONTEXT_DIR), and every variable captured by
// upstream nodes so far.
//
// The runner honors step.Timeout (a per-node context deadline), captures bounded
// combined output, extracts the process exit code, and reports success/failure.
// ContinueOnError is NOT applied here — it only influences how the caller folds
// node results into the overall run state.
func newNodeRunner(repoPath string, event protocol.EventMsg, replay ReplayDispatcher) NodeRunner {
	return func(ctx context.Context, step DAGStep, rc *RunContext) NodeResult {
		nodeCtx := ctx
		if step.Timeout > 0 {
			var cancel context.CancelFunc
			nodeCtx, cancel = context.WithTimeout(ctx, step.Timeout)
			defer cancel()
		}

		log.Info().
			Str("repo", event.Repo).
			Str("node", step.Key).
			Str("type", normalizeActionType(step.Type)).
			Msgf("Event %s on %s: running node %s", eventLabel(event), event.Repo, step.Key)

		res := runWorkflowAction(nodeCtx, repoPath, event, step, rc, replay)
		if res.State == NodeStateFailure {
			log.Error().
				Str("node", step.Key).
				Str("output", res.Output).
				Msgf("Event %s on %s: node %s failed", eventLabel(event), event.Repo, step.Key)
			return res
		}

		if res.Output != "" {
			log.Info().Str("node", step.Key).Msgf("Event %s on %s: node %s output: %s", eventLabel(event), event.Repo, step.Key, res.Output)
		}
		return res
	}
}

// exitCodeFromError extracts a process exit code from an *exec.ExitError,
// returning 1 for non-exit errors (e.g. context timeout or spawn failure).
func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
