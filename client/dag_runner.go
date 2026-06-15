package client

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

// maxNodeOutputBytes bounds the combined output captured per node before it is
// stored or streamed. It mirrors the persistence bound in workflow_store.go.
const maxNodeOutputBytes = 65536

// newNodeRunner returns a NodeRunner that executes each step's command via
// `sh -c` in the checked-out repo directory. The command environment combines
// the shared EVENTIC_* variables (buildHookEnv), the per-run context dir
// (EVENTIC_CONTEXT_DIR), and every variable captured by upstream nodes so far.
//
// The runner honors step.Timeout (a per-node context deadline), captures bounded
// combined output, extracts the process exit code, and reports success/failure.
// ContinueOnError is NOT applied here — it only influences how the caller folds
// node results into the overall run state.
func newNodeRunner(repoPath string, event protocol.EventMsg) NodeRunner {
	return func(ctx context.Context, step DAGStep, rc *RunContext) NodeResult {
		nodeCtx := ctx
		if step.Timeout > 0 {
			var cancel context.CancelFunc
			nodeCtx, cancel = context.WithTimeout(ctx, step.Timeout)
			defer cancel()
		}

		extra := []string{"EVENTIC_CONTEXT_DIR=" + rc.WorkspaceDir}
		for name, value := range rc.Vars {
			extra = append(extra, name+"="+value)
		}

		cmd := exec.CommandContext(nodeCtx, "sh", "-c", step.Command)
		cmd.Dir = repoPath
		if len(rc.BaseEnv) > 0 {
			cmd.Env = append(append([]string{}, rc.BaseEnv...), buildHookEnv(repoPath, event, extra...)...)
		} else {
			cmd.Env = buildHookEnv(repoPath, event, extra...)
		}

		log.Info().
			Str("repo", event.Repo).
			Str("node", step.Key).
			Msgf("Event %s on %s: running node %s", eventLabel(event), event.Repo, step.Key)
		log.Debug().Str("command", step.Command).Str("dir", repoPath).Msg("node command detail")

		out, err := cmd.CombinedOutput()
		output := trimString(strings.TrimSpace(string(out)), maxNodeOutputBytes)
		if err != nil {
			log.Error().
				Err(err).
				Str("node", step.Key).
				Str("output", output).
				Msgf("Event %s on %s: node %s failed", eventLabel(event), event.Repo, step.Key)
			return NodeResult{
				Key:      step.Key,
				State:    NodeStateFailure,
				ExitCode: exitCodeFromError(err),
				Output:   output,
			}
		}

		if output != "" {
			log.Info().Str("node", step.Key).Msgf("Event %s on %s: node %s output: %s", eventLabel(event), event.Repo, step.Key, output)
		}
		return NodeResult{
			Key:      step.Key,
			State:    NodeStateSuccess,
			ExitCode: 0,
			Output:   output,
		}
	}
}

// exitCodeFromError extracts a process exit code from an *exec.ExitError,
// returning 1 for non-exit errors (e.g. context timeout or spawn failure).
func exitCodeFromError(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
