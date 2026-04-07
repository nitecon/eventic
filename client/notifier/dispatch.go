package notifier

import (
	"context"

	"github.com/rs/zerolog/log"
)

// Dispatcher handles asynchronous notification delivery via a buffered channel.
type Dispatcher struct {
	ch       chan dispatchItem
	notifier Notifier
}

type dispatchItem struct {
	ctx context.Context
	n   Notification
}

// NewDispatcher creates a Dispatcher with the given buffer size.
// It starts a background goroutine that drains the channel.
func NewDispatcher(n Notifier, bufSize int) *Dispatcher {
	if bufSize <= 0 {
		bufSize = 100
	}
	d := &Dispatcher{
		ch:       make(chan dispatchItem, bufSize),
		notifier: n,
	}
	go d.drain()
	return d
}

// Send queues a notification for async delivery. Non-blocking if buffer has room;
// drops the notification with a warning if the buffer is full.
func (d *Dispatcher) Send(ctx context.Context, n Notification) {
	select {
	case d.ch <- dispatchItem{ctx: ctx, n: n}:
	default:
		log.Warn().
			Str("repo", n.Repo).
			Str("hook", n.HookName).
			Msg("notification buffer full, dropping notification")
	}
}

// Close signals the dispatcher to stop after draining remaining items.
func (d *Dispatcher) Close() {
	close(d.ch)
}

func (d *Dispatcher) drain() {
	for item := range d.ch {
		if err := d.notifier.Notify(item.ctx, item.n); err != nil {
			log.Error().Err(err).
				Str("repo", item.n.Repo).
				Str("hook", item.n.HookName).
				Msg("async notification failed")
		}
	}
}
