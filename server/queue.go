package server

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/coder/websocket"
)

var (
	queueMutex   sync.Mutex
	messageQueue [][]byte
)

func addToQueue(data []byte) {
	queueMutex.Lock()
	defer queueMutex.Unlock()
	messageQueue = append(messageQueue, data)
	log.Debug().Int("queue_size", len(messageQueue)).Msg("event queued")
}

// drainQueueForClient sends queued messages that match the client's patterns.
func drainQueueForClient(conn *websocket.Conn, patterns []string) {
	queueMutex.Lock()
	defer queueMutex.Unlock()

	remaining := make([][]byte, 0, len(messageQueue))
	for _, msg := range messageQueue {
		var env struct{ Repo string }
		if err := json.Unmarshal(msg, &env); err != nil {
			remaining = append(remaining, msg)
			continue
		}
		if matchesAny(env.Repo, patterns) {
			if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
				log.Error().Err(err).Msg("failed to drain queue message")
				remaining = append(remaining, msg)
				continue
			}
			log.Debug().Str("repo", env.Repo).Msg("drained queued event to client")
		} else {
			remaining = append(remaining, msg)
		}
	}
	messageQueue = remaining
}
