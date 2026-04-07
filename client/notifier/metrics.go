package notifier

import (
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

// Metrics tracks notification send/fail/retry counts per notifier.
type Metrics struct {
	mu       sync.RWMutex
	counters map[string]*ChannelMetrics
}

// ChannelMetrics holds counters for a single notifier channel.
type ChannelMetrics struct {
	Sent    atomic.Int64
	Failed  atomic.Int64
	Retried atomic.Int64
}

// NewMetrics creates a new Metrics tracker.
func NewMetrics() *Metrics {
	return &Metrics{
		counters: make(map[string]*ChannelMetrics),
	}
}

func (m *Metrics) channel(name string) *ChannelMetrics {
	m.mu.RLock()
	c, ok := m.counters[name]
	m.mu.RUnlock()
	if ok {
		return c
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Double-check after acquiring write lock.
	if c, ok = m.counters[name]; ok {
		return c
	}
	c = &ChannelMetrics{}
	m.counters[name] = c
	return c
}

// RecordSent increments the sent counter for the named notifier.
func (m *Metrics) RecordSent(name string) {
	m.channel(name).Sent.Add(1)
}

// RecordFailed increments the failed counter for the named notifier.
func (m *Metrics) RecordFailed(name string) {
	m.channel(name).Failed.Add(1)
}

// RecordRetried increments the retried counter for the named notifier.
func (m *Metrics) RecordRetried(name string) {
	m.channel(name).Retried.Add(1)
}

// Snapshot holds a point-in-time copy of metrics for a single channel.
type Snapshot struct {
	Name    string
	Sent    int64
	Failed  int64
	Retried int64
}

// Snapshots returns a point-in-time copy of all channel metrics.
func (m *Metrics) Snapshots() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Snapshot, 0, len(m.counters))
	for name, c := range m.counters {
		out = append(out, Snapshot{
			Name:    name,
			Sent:    c.Sent.Load(),
			Failed:  c.Failed.Load(),
			Retried: c.Retried.Load(),
		})
	}
	return out
}

// LogSummary writes a summary of all channel metrics at info level.
func (m *Metrics) LogSummary() {
	for _, s := range m.Snapshots() {
		log.Info().
			Str("notifier", s.Name).
			Int64("sent", s.Sent).
			Int64("failed", s.Failed).
			Int64("retried", s.Retried).
			Msg("notification metrics")
	}
}
