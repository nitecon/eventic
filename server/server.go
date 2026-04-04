package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
	"nhooyr.io/websocket"
)

type Config struct {
	WebhookSecret string
	ClientTokens  map[string]bool
	ListenAddr    string
}

func Start(cfg Config) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", webhookHandler(cfg))
	mux.HandleFunc("/ws", wsHandler(cfg))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Info().Str("addr", cfg.ListenAddr).Msg("server starting")
	return http.ListenAndServe(cfg.ListenAddr, mux)
}

func webhookHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		sig := r.Header.Get("X-Hub-Signature-256")
		if !validateHMAC(body, sig, []byte(cfg.WebhookSecret)) {
			log.Warn().Msg("invalid webhook signature")
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		githubEvent := r.Header.Get("X-GitHub-Event")
		deliveryID := r.Header.Get("X-GitHub-Delivery")

		event, err := parseWebhook(githubEvent, deliveryID, body)
		if err != nil {
			log.Error().Err(err).Msg("failed to parse webhook")
			http.Error(w, "parse error", http.StatusBadRequest)
			return
		}

		log.Info().
			Str("event", githubEvent).
			Str("repo", event.Repo).
			Str("ref", event.Ref).
			Str("delivery", deliveryID).
			Msg("webhook received")

		EventChannel <- *event

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"accepted"}`))
	}
}

func validateHMAC(payload []byte, signature string, secret []byte) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return hmac.Equal(sig, mac.Sum(nil))
}

func parseWebhook(eventType, deliveryID string, body []byte) (*protocol.EventMsg, error) {
	var raw struct {
		Repository struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
		Ref    string `json:"ref"`
		Action string `json:"action"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest *struct {
			Number int `json:"number"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
	}

	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal webhook: %w", err)
	}

	event := &protocol.EventMsg{
		MsgType:     "Event",
		DeliveryID:  deliveryID,
		GitHubEvent: eventType,
		Repo:        raw.Repository.FullName,
		Ref:         raw.Ref,
		Action:      raw.Action,
		Sender:      raw.Sender.Login,
		CloneURL:    raw.Repository.CloneURL,
		Payload:     body,
	}

	if raw.PullRequest != nil {
		event.PRNumber = raw.PullRequest.Number
		event.Ref = raw.PullRequest.Head.Ref
	}

	return event, nil
}

func wsHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			log.Error().Err(err).Msg("websocket accept failed")
			return
		}
		defer func() {
			RemoveClient(conn)
			conn.Close(websocket.StatusNormalClosure, "closing")
		}()

		_, authData, err := conn.Read(r.Context())
		if err != nil {
			log.Error().Err(err).Msg("failed to read auth message")
			return
		}

		var auth protocol.AuthMsg
		if err := json.Unmarshal(authData, &auth); err != nil || auth.MsgType != "Auth" {
			resp, _ := json.Marshal(protocol.AuthResult{MsgType: "AuthFail", Reason: "invalid auth message"})
			conn.Write(r.Context(), websocket.MessageText, resp)
			return
		}

		if !cfg.ClientTokens[auth.Token] {
			resp, _ := json.Marshal(protocol.AuthResult{MsgType: "AuthFail", Reason: "invalid token"})
			conn.Write(r.Context(), websocket.MessageText, resp)
			return
		}

		RegisterClient(conn, auth.ClientID)
		resp, _ := json.Marshal(protocol.AuthResult{MsgType: "AuthOK"})
		conn.Write(r.Context(), websocket.MessageText, resp)

		for {
			_, msg, err := conn.Read(r.Context())
			if err != nil {
				log.Debug().Err(err).Str("client", auth.ClientID).Msg("client read error")
				return
			}

			var env protocol.Envelope
			if err := json.Unmarshal(msg, &env); err != nil {
				log.Error().Err(err).Msg("invalid message")
				continue
			}

			switch env.MsgType {
			case "Subscribe":
				var sub protocol.SubscribeMsg
				if err := json.Unmarshal(msg, &sub); err == nil {
					SetSubscription(conn, sub.Patterns)
				}
			case "Status":
				var status protocol.StatusMsg
				if err := json.Unmarshal(msg, &status); err == nil {
					log.Info().
						Str("delivery", status.DeliveryID).
						Str("state", status.State).
						Str("desc", status.Description).
						Msg("client status report")
				}
			case "Pong":
				// heartbeat acknowledged
			default:
				log.Warn().Str("type", env.MsgType).Msg("unknown message type from client")
			}
		}
	}
}
