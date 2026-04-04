package server

import (
	"context"
	"encoding/json"
	"path"
	"sync"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
	"github.com/coder/websocket"
)

// ClientConn represents a connected client.
type ClientConn struct {
	Conn     *websocket.Conn
	ID       string
	Patterns []string
}

var (
	connectedClients = make(map[*websocket.Conn]*ClientConn)
	clientMutex      = new(sync.RWMutex)
	EventChannel     = make(chan protocol.EventMsg, 100)
)

func init() {
	go broadcastToClients()
}

// broadcastToClients reads from EventChannel and routes to matching clients.
func broadcastToClients() {
	for event := range EventChannel {
		data, err := json.Marshal(event)
		if err != nil {
			log.Error().Err(err).Msg("failed to marshal event")
			continue
		}

		clientMutex.RLock()
		sent := 0
		for _, cc := range connectedClients {
			if !matchesAny(event.Repo, cc.Patterns) {
				continue
			}
			err := cc.Conn.Write(context.Background(), websocket.MessageText, data)
			if err != nil {
				log.Error().Err(err).Str("client", cc.ID).Msg("failed to write to client")
				continue
			}
			sent++
			log.Debug().Str("client", cc.ID).Str("repo", event.Repo).Msg("event sent")
		}
		clientMutex.RUnlock()

		if sent == 0 {
			log.Warn().Str("repo", event.Repo).Msg("no clients matched, queueing event")
			addToQueue(data)
		}
	}
}

// matchesAny checks if repo matches any of the glob patterns.
func matchesAny(repo string, patterns []string) bool {
	for _, p := range patterns {
		if matched, _ := path.Match(p, repo); matched {
			return true
		}
	}
	return false
}

// RegisterClient adds a client after successful auth.
func RegisterClient(conn *websocket.Conn, id string) {
	clientMutex.Lock()
	connectedClients[conn] = &ClientConn{Conn: conn, ID: id}
	clientMutex.Unlock()
	log.Info().Str("client", id).Msg("client registered")
}

// SetSubscription updates the client's subscription patterns.
func SetSubscription(conn *websocket.Conn, patterns []string) {
	clientMutex.Lock()
	if cc, ok := connectedClients[conn]; ok {
		cc.Patterns = patterns
		log.Info().Str("client", cc.ID).Strs("patterns", patterns).Msg("subscription updated")
		go drainQueueForClient(conn, patterns)
	}
	clientMutex.Unlock()
}

// RemoveClient removes a disconnected client.
func RemoveClient(conn *websocket.Conn) {
	clientMutex.Lock()
	if cc, ok := connectedClients[conn]; ok {
		log.Info().Str("client", cc.ID).Msg("client disconnected")
		delete(connectedClients, conn)
	}
	clientMutex.Unlock()
}
