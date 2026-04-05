package client

import (
	"context"
	"encoding/json"
	"runtime"
	"time"

	"github.com/coder/websocket"
	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

var repoLocks = NewRepoLocks()

type Config struct {
	Relay      string   `yaml:"relay"`
	Token      string   `yaml:"token"`
	ClientID   string   `yaml:"client_id"`
	ReposDir   string   `yaml:"repos_dir"`
	Subscribe  []string `yaml:"subscribe"`
	AutoUpdate bool     `yaml:"auto-update"`
	AutoCheck  *bool    `yaml:"auto-check"`
	MaxWorkers int      `yaml:"max-workers"`
	LogLevel   string   `yaml:"log-level"`
	GlobalHooks struct {
		Pre  string `yaml:"pre"`
		Post string `yaml:"post"`
	} `yaml:"global-hooks"`
	GlobalIgnorePre   []string `yaml:"global-ignore-pre"`
	GlobalIgnorePost  []string `yaml:"global-ignore-post"`
	GlobalAllowedPre  []string `yaml:"global-allowed-pre"`
	GlobalAllowedPost []string `yaml:"global-allowed-post"`
}

// Run connects to the relay and processes events. Reconnects on failure.
func Run(ctx context.Context, cfg Config) {
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

		err := connect(ctx, cfg, workerSem)
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

func connect(ctx context.Context, cfg Config, workerSem chan struct{}) error {
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
			go processEvent(ctx, conn, cfg, event, workerSem)

		case "Ping":
			pong, _ := json.Marshal(protocol.Envelope{MsgType: "Pong"})
			conn.Write(ctx, websocket.MessageText, pong)
		}
	}
}

func processEvent(ctx context.Context, conn *websocket.Conn, cfg Config, event protocol.EventMsg, workerSem chan struct{}) {
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

	repoPath, err := EnsureRepo(cfg.ReposDir, event.Repo, event.CloneURL)
	if err != nil {
		state = "failure"
		desc = err.Error()
		log.Error().Err(err).Str("repo", event.Repo).Msg("repo sync failed")
	} else {
		hooks := DiscoverHooks(repoPath, event, cfg.GlobalHooks.Pre, cfg.GlobalHooks.Post)

		// Execution order:
		// 1. Global pre hook (runs for all events)
		// 2. Event-specific pre hook
		// 3. git checkout
		// 4. Event-specific post hook
		// 5. Global post hook (runs for all events)

		if hooks.Pre != "" && shouldRunGlobalHook(event.GitHubEvent, event.Action, cfg.GlobalAllowedPre, cfg.GlobalIgnorePre) {
			if err := RunHook(ctx, repoPath, hooks.Pre, "global:pre", event); err != nil {
				state = "failure"
				desc = err.Error()
			}
		} else if hooks.Pre != "" {
			log.Debug().Str("event", eventLabel(event)).Msg("skipping global pre hook (filtered)")
		}

		if hooks.EventPre != "" {
			if err := RunHook(ctx, repoPath, hooks.EventPre, "event:pre", event); err != nil {
				state = "failure"
				desc = err.Error()
			}
		}

		if err := Checkout(repoPath, event); err != nil {
			state = "failure"
			desc = err.Error()
			log.Error().Err(err).Msgf("Event %s on %s: checkout failed", eventLabel(event), event.Repo)
		} else {
			// Event-specific post hook
			if hooks.EventPost != "" {
				if out, err := RunHookWithOutput(ctx, repoPath, hooks.EventPost, "event:post", event); err != nil {
					state = "failure"
					desc = err.Error()
				} else {
					desc = out
				}
			}

			// Global post hook (runs if checkout succeeded and not ignored)
			if hooks.Post != "" && shouldRunGlobalHook(event.GitHubEvent, event.Action, cfg.GlobalAllowedPost, cfg.GlobalIgnorePost) {
				if out, err := RunHookWithOutput(ctx, repoPath, hooks.Post, "global:post", event); err != nil {
					if state == "success" {
						state = "failure"
						desc = err.Error()
					}
				} else if desc == "" {
					desc = out
				}
			} else if hooks.Post != "" {
				log.Debug().Str("event", eventLabel(event)).Msg("skipping global post hook (filtered)")
			}
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
