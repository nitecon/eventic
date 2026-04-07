package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Notification represents a rich notification message.
type Notification struct {
	Repo       string
	Event      string
	Action     string
	HookName   string
	Message    string
	Stdout     string
	Sender     string
	DeliveryID string
	State      string          // success, failure, pending
	RawPayload json.RawMessage // full GitHub webhook payload for template access
}

// Notifier is the interface for sending notifications.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
	Ping(ctx context.Context) error
	Name() string
	// Metrics returns the metrics tracker for this notifier tree.
	GetMetrics() *Metrics
}

// Factory is a function that creates a Notifier from generic configuration.
type Factory func(cfg map[string]interface{}) (Notifier, error)

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Factory)
)

// Register adds a new notifier factory to the registry.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[strings.ToLower(name)] = factory
}

// Config holds the configuration for all supported notifiers.
type Config struct {
	Enabled  []string                          `yaml:"enabled"`
	Settings map[string]map[string]interface{} `yaml:"settings"`
}

// Validate checks that all enabled notifiers have valid configuration.
// Returns errors for each misconfigured notifier without creating them.
func (c Config) Validate() []error {
	var errs []error
	for _, name := range c.Enabled {
		registryMu.RLock()
		factory, ok := registry[strings.ToLower(name)]
		registryMu.RUnlock()

		if !ok {
			errs = append(errs, fmt.Errorf("notifier %q: not registered", name))
			continue
		}

		settings := c.Settings[name]
		if settings == nil {
			settings = make(map[string]interface{})
		}
		if _, err := factory(settings); err != nil {
			errs = append(errs, fmt.Errorf("notifier %q: %w", name, err))
		}
	}
	return errs
}

// RetryConfig controls retry behavior for notifications.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
}

// DefaultRetryConfig returns sensible retry defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   500 * time.Millisecond,
	}
}

// MultiNotifier bundles multiple notifiers together with retry and metrics.
type MultiNotifier struct {
	notifiers []Notifier
	retry     RetryConfig
	metrics   *Metrics
}

func (m *MultiNotifier) Name() string         { return "multi" }
func (m *MultiNotifier) GetMetrics() *Metrics  { return m.metrics }

func (m *MultiNotifier) Ping(ctx context.Context) error {
	var errs []string
	for _, child := range m.notifiers {
		if err := child.Ping(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", child.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("ping errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *MultiNotifier) Notify(ctx context.Context, n Notification) error {
	var errs []string
	for _, child := range m.notifiers {
		if err := m.notifyWithRetry(ctx, child, n); err != nil {
			m.metrics.RecordFailed(child.Name())
			log.Error().Err(err).Str("notifier", child.Name()).Msg("notification failed after retries")
			errs = append(errs, fmt.Sprintf("%s: %v", child.Name(), err))
		} else {
			m.metrics.RecordSent(child.Name())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("multi-notification errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *MultiNotifier) notifyWithRetry(ctx context.Context, child Notifier, n Notification) error {
	var lastErr error
	for attempt := 0; attempt < m.retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			m.metrics.RecordRetried(child.Name())
			delay := m.retry.BaseDelay * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		lastErr = child.Notify(ctx, n)
		if lastErr == nil {
			return nil
		}

		// Only retry on transient errors (contains status code hints)
		if !isRetryable(lastErr) {
			return lastErr
		}
		log.Warn().Err(lastErr).Str("notifier", child.Name()).Int("attempt", attempt+1).Msg("notification failed, retrying")
	}
	return lastErr
}

// isRetryable returns true for errors that suggest a transient failure.
func isRetryable(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "500") ||
		strings.Contains(msg, "502") ||
		strings.Contains(msg, "503") ||
		strings.Contains(msg, "504") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection refused")
}

// NewNotifier creates a MultiNotifier based on the enabled list and settings.
func NewNotifier(cfg Config) Notifier {
	metrics := NewMetrics()
	multi := &MultiNotifier{
		retry:   DefaultRetryConfig(),
		metrics: metrics,
	}

	for _, name := range cfg.Enabled {
		registryMu.RLock()
		factory, ok := registry[strings.ToLower(name)]
		registryMu.RUnlock()

		if !ok {
			log.Warn().Str("notifier", name).Msg("notifier type not registered")
			continue
		}

		settings := cfg.Settings[name]
		if settings == nil {
			settings = make(map[string]interface{})
		}
		n, err := factory(settings)
		if err != nil {
			log.Error().Err(err).Str("notifier", name).Msg("failed to initialize notifier")
			continue
		}
		multi.notifiers = append(multi.notifiers, n)
	}

	if len(multi.notifiers) == 0 {
		return &NoopNotifier{metrics: metrics}
	}
	return multi
}

// NoopNotifier does nothing.
type NoopNotifier struct {
	metrics *Metrics
}

func (n *NoopNotifier) Notify(ctx context.Context, notification Notification) error { return nil }
func (n *NoopNotifier) Ping(ctx context.Context) error                             { return nil }
func (n *NoopNotifier) Name() string                                               { return "noop" }
func (n *NoopNotifier) GetMetrics() *Metrics                                       { return n.metrics }

// repoSlug converts "org/repo" to "org-repo" for safe channel naming.
func repoSlug(repo string) string {
	return strings.ReplaceAll(strings.ToLower(repo), "/", "-")
}
