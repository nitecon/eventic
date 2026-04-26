package client

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/coder/websocket"
	"github.com/nitecon/eventic/client/notifier"
	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

var repoLocks = NewRepoLocks()

type Config struct {
	Relay       string   `yaml:"relay"`
	Token       string   `yaml:"token"`
	ClientID    string   `yaml:"client_id"`
	ReposDir    string   `yaml:"repos_dir"`
	Subscribe   []string `yaml:"subscribe"`
	AutoUpdate  bool     `yaml:"auto-update"`
	AutoCheck   *bool    `yaml:"auto-check"`
	MaxWorkers  int      `yaml:"max-workers"`
	LogLevel    string   `yaml:"log-level"`
	GlobalHooks struct {
		Pre      string   `yaml:"pre"`
		Post     string   `yaml:"post"`
		Notify   string   `yaml:"notify"`
		NotifyOn []string `yaml:"notify_on"`
	} `yaml:"global-hooks"`
	GlobalIgnorePre   []string        `yaml:"global-ignore-pre"`
	GlobalIgnorePost  []string        `yaml:"global-ignore-post"`
	GlobalAllowedPre  []string        `yaml:"global-allowed-pre"`
	GlobalAllowedPost []string        `yaml:"global-allowed-post"`
	DefaultNotify     string          `yaml:"default-notify"`
	DefaultNotifyOn   []string        `yaml:"default-notify-on"`
	Notifier          notifier.Config `yaml:"notifier"`
	RequireApproval   bool            `yaml:"require_approval"`
	ApprovalsPath     string          `yaml:"approvals_path"`
	Web               WebConfig       `yaml:"web"`
	State             StateConfig     `yaml:"state"`
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
	defer projectStore.Close()
	projectStore.SeedFromReposDir(ctx, cfg.ReposDir)
	if cfg.Web.Enabled {
		go func() {
			if err := StartWebConsole(ctx, cfg.Web, webLog, projectStore); err != nil {
				log.Error().Err(err).Msg("web console stopped")
			}
		}()
	}

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
		projectStore.UpdateOutput(ctx, event.Repo, "failure", "")

		status, _ := json.Marshal(protocol.StatusMsg{
			MsgType:     "Status",
			DeliveryID:  event.DeliveryID,
			State:       state,
			Description: desc,
		})
		conn.Write(ctx, websocket.MessageText, status)
		return
	}

	repoPath, err := EnsureRepo(cfg.ReposDir, event.Repo, event.CloneURL)
	if err != nil {
		state = "failure"
		desc = err.Error()
		log.Error().Err(err).Str("repo", event.Repo).Msg("repo sync failed")
	} else {
		hooks := DiscoverHooks(repoPath, event, cfg.GlobalHooks.Pre, cfg.GlobalHooks.Post, cfg.GlobalHooks.Notify, cfg.GlobalHooks.NotifyOn)

		// Apply default notification template as fallback for global notify.
		if hooks.Notify == "" && cfg.DefaultNotify != "" {
			hooks.Notify = cfg.DefaultNotify
			if len(hooks.NotifyOn) == 0 {
				hooks.NotifyOn = cfg.DefaultNotifyOn
			}
		}

		// Execution order:
		// 1. Global pre hook
		// 2. Event-specific pre hook
		// 3. git checkout
		// 4. Event-specific post hook
		// 5. Event-specific notify (with post-hook output, filtered by notify_on)
		// 6. Global post hook
		// 7. Global notify (final summary, filtered by notify_on)

		hooksExecuted := false
		var lastOut string

		if hooks.Pre != "" && shouldRunGlobalHook(event.GitHubEvent, event.Action, cfg.GlobalAllowedPre, cfg.GlobalIgnorePre) {
			sendStartNotification(ctx, dispatch, "global:pre", event)
			hooksExecuted = true
			webLog.StartHook(event.DeliveryID, "global:pre")
			if out, err := RunHookWithOutput(ctx, repoPath, hooks.Pre, "global:pre", event); err != nil {
				state = "failure"
				desc = err.Error()
				lastOut = out
				webLog.FinishHook(event.DeliveryID, "global:pre", "failure", out)
				projectStore.UpdateOutput(ctx, event.Repo, "failure", out)
			} else {
				lastOut = out
				webLog.FinishHook(event.DeliveryID, "global:pre", "success", out)
				projectStore.UpdateOutput(ctx, event.Repo, "success", out)
			}
		} else if hooks.Pre != "" {
			log.Debug().Str("event", eventLabel(event)).Msg("skipping global pre hook (filtered)")
		}

		if hooks.EventPre != "" {
			sendStartNotification(ctx, dispatch, "event:pre", event)
			hooksExecuted = true
			webLog.StartHook(event.DeliveryID, "event:pre")
			if out, err := RunHookWithOutput(ctx, repoPath, hooks.EventPre, "event:pre", event); err != nil {
				state = "failure"
				desc = err.Error()
				lastOut = out
				webLog.FinishHook(event.DeliveryID, "event:pre", "failure", out)
				projectStore.UpdateOutput(ctx, event.Repo, "failure", out)
			} else {
				lastOut = out
				webLog.FinishHook(event.DeliveryID, "event:pre", "success", out)
				projectStore.UpdateOutput(ctx, event.Repo, "success", out)
			}
		}

		if err := Checkout(repoPath, event); err != nil {
			state = "failure"
			desc = err.Error()
			log.Error().Err(err).Msgf("Event %s on %s: checkout failed", eventLabel(event), event.Repo)
		} else {
			currentRef, currentHash, err := CurrentGitState(repoPath)
			if err != nil {
				log.Warn().Err(err).Str("repo", event.Repo).Msg("failed to read current git state")
			} else {
				projectStore.UpdateGitState(ctx, event.Repo, currentRef, currentHash)
			}

			// Event-specific post hook
			eventPostState := "success"
			var eventPostOut string
			if hooks.EventPost != "" {
				sendStartNotification(ctx, dispatch, "event:post", event)
				hooksExecuted = true
				webLog.StartHook(event.DeliveryID, "event:post")
				if out, err := RunHookWithOutput(ctx, repoPath, hooks.EventPost, "event:post", event); err != nil {
					state = "failure"
					desc = err.Error()
					eventPostState = "failure"
					eventPostOut = out
					webLog.FinishHook(event.DeliveryID, "event:post", "failure", out)
					projectStore.UpdateOutput(ctx, event.Repo, "failure", out)
				} else {
					desc = out
					eventPostOut = out
					webLog.FinishHook(event.DeliveryID, "event:post", "success", out)
					projectStore.UpdateOutput(ctx, event.Repo, "success", out)
				}
			}

			// Event-specific notify (filtered by notify_on, only if hooks ran)
			if hooksExecuted && hooks.EventNotify != "" && ShouldNotify(hooks.EventNotifyOn, eventPostState) {
				sendNotification(ctx, dispatch, "event:post", hooks.EventNotify, eventPostOut, eventPostState, hooks.EventNotifyOn, event)
			}

			// Global post hook
			if hooks.Post != "" && shouldRunGlobalHook(event.GitHubEvent, event.Action, cfg.GlobalAllowedPost, cfg.GlobalIgnorePost) {
				sendStartNotification(ctx, dispatch, "global:post", event)
				hooksExecuted = true
				webLog.StartHook(event.DeliveryID, "global:post")
				if out, err := RunHookWithOutput(ctx, repoPath, hooks.Post, "global:post", event); err != nil {
					if state == "success" {
						state = "failure"
						desc = err.Error()
					}
					lastOut = out
					webLog.FinishHook(event.DeliveryID, "global:post", "failure", out)
					projectStore.UpdateOutput(ctx, event.Repo, "failure", out)
				} else if desc == "" {
					desc = out
					lastOut = out
					webLog.FinishHook(event.DeliveryID, "global:post", "success", out)
					projectStore.UpdateOutput(ctx, event.Repo, "success", out)
				} else {
					lastOut = out
					webLog.FinishHook(event.DeliveryID, "global:post", "success", out)
					projectStore.UpdateOutput(ctx, event.Repo, "success", out)
				}
			} else if hooks.Post != "" {
				log.Debug().Str("event", eventLabel(event)).Msg("skipping global post hook (filtered)")
			}
		}

		// Global notify (filtered by notify_on, only if hooks ran)
		if hooksExecuted && hooks.Notify != "" && ShouldNotify(hooks.NotifyOn, state) {
			sendNotification(ctx, dispatch, "global:summary", hooks.Notify, lastOut, state, hooks.NotifyOn, event)
		} else if !hooksExecuted && hooks.Notify != "" {
			log.Debug().Str("event", eventLabel(event)).Msg("skipping notification (no hooks executed)")
		}
	}

	status, _ := json.Marshal(protocol.StatusMsg{
		MsgType:     "Status",
		DeliveryID:  event.DeliveryID,
		State:       state,
		Description: desc,
	})
	conn.Write(ctx, websocket.MessageText, status)

	log.Debug().
		Str("repo", event.Repo).
		Str("state", state).
		Msg("event processed")
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

// sendStartNotification sends a built-in "Starting <hookLabel>" notification
// with state "pending" before a hook begins execution.
func sendStartNotification(ctx context.Context, dispatch *notifier.Dispatcher, hookLabel string, event protocol.EventMsg) {
	n := notifier.Notification{
		Repo:       event.Repo,
		Event:      event.GitHubEvent,
		Action:     event.Action,
		HookName:   hookLabel,
		Message:    fmt.Sprintf("Starting %s", hookLabel),
		Sender:     event.Sender,
		DeliveryID: event.DeliveryID,
		State:      "pending",
		RawPayload: event.Payload,
	}

	log.Debug().Str("hook", hookLabel).Msgf("Event %s on %s: queuing start notification", eventLabel(event), event.Repo)
	dispatch.Send(ctx, n)
}
