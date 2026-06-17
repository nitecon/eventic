package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/nitecon/eventic/client/notifier"
	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

var repoLocks = NewRepoLocks()

type Config struct {
	Relay           string          `yaml:"relay"`
	Token           string          `yaml:"token"`
	ClientID        string          `yaml:"client_id"`
	ReposDir        string          `yaml:"repos_dir"`
	Subscribe       []string        `yaml:"subscribe"`
	AutoUpdate      bool            `yaml:"auto-update"`
	AutoCheck       *bool           `yaml:"auto-check"`
	MaxWorkers      int             `yaml:"max-workers"`
	MaxNodeWorkers  int             `yaml:"max-node-workers"`
	LogLevel        string          `yaml:"log-level"`
	DefaultNotify   string          `yaml:"default-notify"`
	DefaultNotifyOn []string        `yaml:"default-notify-on"`
	Notifier        notifier.Config `yaml:"notifier"`
	RequireApproval bool            `yaml:"require_approval"`
	ApprovalsPath   string          `yaml:"approvals_path"`
	Web             WebConfig       `yaml:"web"`
	State           StateConfig     `yaml:"state"`
}

// Run connects to the relay and processes events. Reconnects on failure.
func Run(ctx context.Context, cfg Config) {
	// Validate notifier configuration before starting.
	if errs := cfg.Notifier.Validate(); len(errs) > 0 {
		for _, err := range errs {
			log.Error().Err(err).Msg("notifier config error")
		}
		log.Fatal().Msg("fix notifier configuration before starting")
	}

	n := notifier.NewNotifier(cfg.Notifier)
	webLog := NewExecutionLog(cfg.Web)
	projectStore, err := OpenProjectStore(ctx, cfg.State)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open project state database")
	}
	if projectStore == nil && cfg.Web.Enabled {
		projectStore, err = OpenMemoryProjectStore(ctx)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to open in-memory project state database")
		}
	}
	defer projectStore.Close()
	projectStore.SeedFromReposDir(ctx, cfg.ReposDir, cfg)

	// Health-check all notifiers at startup.
	if err := n.Ping(ctx); err != nil {
		log.Warn().Err(err).Msg("notifier health check failed — notifications may not work")
	} else {
		log.Info().Msg("notifier health check passed")
	}

	// Async notification dispatch — decouples slow webhooks from event processing.
	dispatch := notifier.NewDispatcher(n, 200)
	defer dispatch.Close()

	// Periodic metrics logging.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if m := n.GetMetrics(); m != nil {
					m.LogSummary()
				}
			}
		}
	}()

	var approvalStore *ApprovalStore
	if cfg.RequireApproval {
		approvalsPath := cfg.ApprovalsPath
		if approvalsPath == "" {
			approvalsPath = "/etc/eventic/approvals.json"
		}
		approvalStore = NewApprovalStore(approvalsPath)
	}

	workers := cfg.MaxWorkers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	workerSem := make(chan struct{}, workers)
	log.Debug().Int("max_workers", workers).Msg("worker pool configured")

	if cfg.Web.Enabled {
		go func() {
			replay := func(_ context.Context, event protocol.EventMsg) {
				go processEvent(ctx, nil, cfg, event, workerSem, dispatch, approvalStore, webLog, projectStore)
			}
			if err := StartWebConsole(ctx, cfg, webLog, projectStore, replay); err != nil {
				log.Error().Err(err).Msg("web console stopped")
			}
		}()
	}

	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := connect(ctx, cfg, workerSem, dispatch, approvalStore, webLog, projectStore)
		if err != nil {
			log.Error().Err(err).Dur("backoff", backoff).Msg("connection lost, reconnecting")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

func connect(ctx context.Context, cfg Config, workerSem chan struct{}, dispatch *notifier.Dispatcher, approvalStore *ApprovalStore, webLog *ExecutionLog, projectStore *ProjectStore) error {
	conn, _, err := websocket.Dial(ctx, cfg.Relay, nil)
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "closing")

	// Disable read limit for large payloads
	conn.SetReadLimit(-1)

	authMsg, _ := json.Marshal(protocol.AuthMsg{
		MsgType:  "Auth",
		Token:    cfg.Token,
		ClientID: cfg.ClientID,
	})
	if err := conn.Write(ctx, websocket.MessageText, authMsg); err != nil {
		return err
	}

	_, resp, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	var authResult protocol.AuthResult
	if err := json.Unmarshal(resp, &authResult); err != nil {
		return err
	}
	if authResult.MsgType == "AuthFail" {
		log.Fatal().Str("reason", authResult.Reason).Msg("auth failed")
	}
	log.Info().Msg("authenticated")

	subMsg, _ := json.Marshal(protocol.SubscribeMsg{
		MsgType:  "Subscribe",
		Patterns: cfg.Subscribe,
	})
	if err := conn.Write(ctx, websocket.MessageText, subMsg); err != nil {
		return err
	}
	log.Info().Strs("patterns", cfg.Subscribe).Msg("subscribed")

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var env protocol.Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			log.Error().Err(err).Msg("invalid message")
			continue
		}

		switch env.MsgType {
		case "Event":
			var event protocol.EventMsg
			if err := json.Unmarshal(msg, &event); err != nil {
				log.Error().Err(err).Msg("failed to parse event")
				continue
			}
			go processEvent(ctx, conn, cfg, event, workerSem, dispatch, approvalStore, webLog, projectStore)

		case "Ping":
			pong, _ := json.Marshal(protocol.Envelope{MsgType: "Pong"})
			conn.Write(ctx, websocket.MessageText, pong)
		}
	}
}

func processEvent(ctx context.Context, conn *websocket.Conn, cfg Config, event protocol.EventMsg, workerSem chan struct{}, dispatch *notifier.Dispatcher, approvalStore *ApprovalStore, webLog *ExecutionLog, projectStore *ProjectStore) {
	// Acquire worker slot (bounded concurrency)
	workerSem <- struct{}{}
	defer func() { <-workerSem }()

	// Acquire per-repo lock — serializes events for the same repo so
	// concurrent checkouts don't collide, while different repos proceed
	// in parallel.
	repoLocks.Lock(event.Repo)
	defer repoLocks.Unlock(event.Repo)

	log.Debug().
		Str("repo", event.Repo).
		Str("event", event.GitHubEvent).
		Str("action", event.Action).
		Str("ref", event.Ref).
		Msg("processing event")

	state := "success"
	desc := ""
	startedEvent := webLog.StartEvent(event)
	projectStore.StartProject(ctx, startedEvent)
	defer func() {
		if finishedEvent := webLog.FinishEvent(event.DeliveryID, state, desc); finishedEvent != nil {
			projectStore.FinishProject(ctx, *finishedEvent)
		}
	}()

	// ── Approval Check ─────────────────────────────────────────────────────────
	if approvalStore != nil && !approvalStore.IsApproved(event.Repo, event.Sender) {
		log.Warn().
			Str("repo", event.Repo).
			Str("sender", event.Sender).
			Msg("blocking event from unapproved source")

		state = "failure"
		desc = "approval required"

		sendNotification(ctx, dispatch, "approval:required",
			fmt.Sprintf("Blocked event from unapproved source. To approve, run:\n`eventic-client approve --repo %s` or `eventic-client approve --sender %s`", event.Repo, event.Sender),
			"", "failure", nil, event)
		webLog.AddHook(event.DeliveryID, "approval:required", "failure", "")
		projectStore.UpdateOutput(ctx, event.Repo, event, "approval:required", "failure", "")

		writeStatus(ctx, conn, protocol.StatusMsg{
			MsgType:     "Status",
			DeliveryID:  event.DeliveryID,
			State:       state,
			Description: desc,
		})
		return
	}

	replay := func(replayCtx context.Context, replayEvent protocol.EventMsg) {
		go processEvent(replayCtx, nil, cfg, replayEvent, workerSem, dispatch, approvalStore, webLog, projectStore)
	}
	state, desc = runEventWorkflow(ctx, cfg, event, dispatch, webLog, projectStore, replay)

	writeStatus(ctx, conn, protocol.StatusMsg{
		MsgType:     "Status",
		DeliveryID:  event.DeliveryID,
		State:       state,
		Description: desc,
	})

	log.Debug().
		Str("repo", event.Repo).
		Str("state", state).
		Msg("event processed")
}

func writeStatus(ctx context.Context, conn *websocket.Conn, status protocol.StatusMsg) {
	if conn == nil {
		return
	}
	data, _ := json.Marshal(status)
	conn.Write(ctx, websocket.MessageText, data)
}

// sendNotification builds a Notification and dispatches it asynchronously.
func sendNotification(ctx context.Context, dispatch *notifier.Dispatcher, hookName, message, stdout, state string, notifyOn []string, event protocol.EventMsg) {
	if message == "" {
		return
	}

	n := notifier.Notification{
		Repo:       event.Repo,
		Event:      event.GitHubEvent,
		Action:     event.Action,
		HookName:   hookName,
		Message:    message,
		Stdout:     stdout,
		Sender:     event.Sender,
		DeliveryID: event.DeliveryID,
		State:      state,
		RawPayload: event.Payload,
	}

	log.Info().Str("hook", hookName).Msgf("Event %s on %s: queuing notification", eventLabel(event), event.Repo)
	dispatch.Send(ctx, n)
}

// runEventWorkflow resolves the workflow for an event, checks out the repo, and
// executes the resolved DAG. It returns the overall terminal state ("success",
// "failure", or "skipped") and a human-readable description. It never panics on
// a missing workflow — that is a benign "skipped" outcome.
func runEventWorkflow(ctx context.Context, cfg Config, event protocol.EventMsg, dispatch *notifier.Dispatcher, webLog *ExecutionLog, projectStore *ProjectStore, replay ReplayDispatcher) (state, desc string) {
	wf, err := projectStore.ResolveWorkflow(ctx, event.Repo, event.GitHubEvent, event.Action)
	if err != nil {
		log.Error().Err(err).Str("repo", event.Repo).Str("event", eventLabel(event)).Msg("workflow resolution failed")
		return "failure", err.Error()
	}
	if wf == nil {
		log.Info().Str("repo", event.Repo).Str("event", eventLabel(event)).Msg("no workflow configured for event — skipping")
		return "skipped", "no workflow configured"
	}

	repoPath, err := EnsureRepo(cfg.ReposDir, event.Repo, event.CloneURL)
	if err != nil {
		log.Error().Err(err).Str("repo", event.Repo).Msg("repo sync failed")
		return "failure", err.Error()
	}
	if err := Checkout(repoPath, event); err != nil {
		log.Error().Err(err).Msgf("Event %s on %s: checkout failed", eventLabel(event), event.Repo)
		return "failure", err.Error()
	}
	currentRef, currentHash, gitErr := CurrentGitState(repoPath)
	if gitErr != nil {
		log.Warn().Err(gitErr).Str("repo", event.Repo).Msg("failed to read current git state")
	} else {
		projectStore.UpdateGitState(ctx, event.Repo, currentRef, currentHash)
	}

	graph, err := BuildGraph(wf.Nodes, wf.Edges)
	if err == nil {
		err = Validate(graph)
	}
	if err != nil {
		log.Error().Err(err).Int64("workflow", wf.ID).Str("repo", event.Repo).Msg("invalid workflow graph")
		recordFailedRun(ctx, projectStore, wf, event, currentRef, currentHash, err.Error())
		return "failure", err.Error()
	}

	return executeWorkflowGraph(ctx, cfg, event, wf, graph, repoPath, currentRef, currentHash, dispatch, webLog, projectStore, replay)
}

// executeWorkflowGraph starts a workflow run, executes the DAG, and folds the
// per-node results into the overall run state. It drives both the persisted run
// records and the live web execution log, and dispatches a run-completion
// notification honoring the configured notify_on filter.
func executeWorkflowGraph(ctx context.Context, cfg Config, event protocol.EventMsg, wf *Workflow, graph Graph, repoPath, ref, hash string, dispatch *notifier.Dispatcher, webLog *ExecutionLog, projectStore *ProjectStore, replay ReplayDispatcher) (state, desc string) {
	runID, err := projectStore.StartRun(ctx, &WorkflowRun{
		WorkflowID: wf.ID,
		Repo:       event.Repo,
		EventType:  eventLabel(event),
		DeliveryID: event.DeliveryID,
		Ref:        ref,
		Hash:       hash,
		Trigger:    deriveTrigger(event),
	})
	if err != nil {
		log.Error().Err(err).Int64("workflow", wf.ID).Msg("failed to start workflow run")
		return "failure", err.Error()
	}

	workspace, err := os.MkdirTemp("", "eventic-run-")
	if err != nil {
		log.Error().Err(err).Msg("failed to create run workspace")
		projectStore.FinishRun(ctx, runID, "failure")
		return "failure", err.Error()
	}
	defer func() {
		if rmErr := os.RemoveAll(workspace); rmErr != nil {
			log.Warn().Err(rmErr).Str("workspace", workspace).Msg("failed to remove run workspace")
		}
	}()

	rc := &RunContext{
		Vars:         make(map[string]string),
		WorkspaceDir: workspace,
		BaseEnv:      os.Environ(),
	}

	// Wrap the real runner so each node also drives the run records and the
	// live web execution log (node_key as the hook name) for SSE streaming.
	runner := newNodeRunner(repoPath, event, replay)
	tracked := func(nodeCtx context.Context, step DAGStep, runCtx *RunContext) NodeResult {
		projectStore.StartRunNode(ctx, runID, step.Key)
		webLog.StartHook(event.DeliveryID, step.Key)

		res := runner(nodeCtx, step, runCtx)

		projectStore.FinishRunNode(ctx, runID, step.Key, res.State, res.ExitCode, res.Output)
		webLog.FinishHook(event.DeliveryID, step.Key, res.State, res.Output)
		projectStore.UpdateOutput(ctx, event.Repo, event, step.Key, res.State, res.Output)
		return res
	}

	workers := cfg.MaxNodeWorkers
	if workers <= 0 {
		workers = 1
	}
	results, execErr := Execute(ctx, graph, rc, tracked, workers)
	if execErr != nil {
		log.Warn().Err(execErr).Int64("run", runID).Msg("workflow execution interrupted")
	}

	// Fold node results: the run fails if any node failed and was not marked
	// continue_on_error. Skipped run-node records are written for skips so the
	// run detail reflects the full graph.
	state = "success"
	desc = ""
	var lastOutput string
	for _, res := range results {
		switch res.State {
		case NodeStateSkipped:
			projectStore.StartRunNode(ctx, runID, res.Key)
			projectStore.FinishRunNode(ctx, runID, res.Key, NodeStateSkipped, 0, "")
		case NodeStateFailure:
			lastOutput = res.Output
			if !graph.Nodes[res.Key].ContinueOnError {
				state = "failure"
				if desc == "" {
					desc = fmt.Sprintf("node %q failed", res.Key)
				}
			}
		case NodeStateSuccess:
			lastOutput = res.Output
		}
	}
	if execErr != nil && state != "failure" {
		state = "failure"
		desc = execErr.Error()
	}

	projectStore.FinishRun(ctx, runID, state)
	projectStore.PruneRuns(ctx, event.Repo, 0)

	// Run-completion notification: honor the configured default notify template
	// and notify_on filter at run granularity, reusing the last node's output.
	if cfg.DefaultNotify != "" && ShouldNotify(cfg.DefaultNotifyOn, state) {
		sendNotification(ctx, dispatch, "workflow:summary", cfg.DefaultNotify, lastOutput, state, cfg.DefaultNotifyOn, event)
	}

	log.Info().
		Int64("run", runID).
		Int64("workflow", wf.ID).
		Str("repo", event.Repo).
		Str("state", state).
		Msg("workflow run complete")
	return state, desc
}

// recordFailedRun persists a synthetic failed run for a workflow whose graph was
// rejected before execution, so the failure is visible in run history.
func recordFailedRun(ctx context.Context, projectStore *ProjectStore, wf *Workflow, event protocol.EventMsg, ref, hash, reason string) {
	runID, err := projectStore.StartRun(ctx, &WorkflowRun{
		WorkflowID: wf.ID,
		Repo:       event.Repo,
		EventType:  eventLabel(event),
		DeliveryID: event.DeliveryID,
		Ref:        ref,
		Hash:       hash,
		Trigger:    deriveTrigger(event),
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to record invalid workflow run")
		return
	}
	log.Debug().Int64("run", runID).Str("reason", reason).Msg("recording failed workflow run for invalid graph")
	projectStore.FinishRun(ctx, runID, "failure")
}

// deriveTrigger classifies how a run was initiated for the workflow_runs.trigger
// column: "comms" for comms events, "manual" for dashboard/manual replays, and
// "webhook" for GitHub-delivered events.
func deriveTrigger(event protocol.EventMsg) string {
	switch {
	case event.GitHubEvent == "comms":
		return "comms"
	case strings.HasPrefix(event.DeliveryID, "manual-"), strings.HasPrefix(event.DeliveryID, "comms-"):
		return "manual"
	default:
		return "webhook"
	}
}
