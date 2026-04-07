package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── Mock notifier for testing ──────────────────────────────────────────────────

type mockNotifier struct {
	name      string
	calls     atomic.Int64
	failUntil atomic.Int64 // fail the first N calls with a retryable error
	lastN     Notification
	mu        sync.Mutex
	pingErr   error
}

func (m *mockNotifier) Name() string         { return m.name }
func (m *mockNotifier) GetMetrics() *Metrics  { return nil }
func (m *mockNotifier) Ping(ctx context.Context) error {
	return m.pingErr
}

func (m *mockNotifier) Notify(ctx context.Context, n Notification) error {
	call := m.calls.Add(1)
	m.mu.Lock()
	m.lastN = n
	m.mu.Unlock()
	if call <= m.failUntil.Load() {
		return fmt.Errorf("error status: 503")
	}
	return nil
}

// ── Registry tests ─────────────────────────────────────────────────────────────

func TestRegisterAndCreate(t *testing.T) {
	Register("test-notifier", func(cfg map[string]interface{}) (Notifier, error) {
		name, _ := cfg["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("test-notifier: name required")
		}
		return &mockNotifier{name: name}, nil
	})

	cfg := Config{
		Enabled: []string{"test-notifier"},
		Settings: map[string]map[string]interface{}{
			"test-notifier": {"name": "mytest"},
		},
	}

	n := NewNotifier(cfg)
	if n.Name() != "multi" {
		t.Fatalf("expected multi notifier, got %s", n.Name())
	}
}

func TestNewNotifierFallsBackToNoop(t *testing.T) {
	cfg := Config{Enabled: []string{}}
	n := NewNotifier(cfg)
	if n.Name() != "noop" {
		t.Fatalf("expected noop, got %s", n.Name())
	}
}

func TestNewNotifierSkipsUnregistered(t *testing.T) {
	cfg := Config{Enabled: []string{"nonexistent-xyz"}}
	n := NewNotifier(cfg)
	if n.Name() != "noop" {
		t.Fatalf("expected noop for unregistered notifier, got %s", n.Name())
	}
}

// ── Config validation ──────────────────────────────────────────────────────────

func TestConfigValidate(t *testing.T) {
	Register("valid-test", func(cfg map[string]interface{}) (Notifier, error) {
		if _, ok := cfg["key"]; !ok {
			return nil, fmt.Errorf("key is required")
		}
		return &mockNotifier{name: "valid-test"}, nil
	})

	t.Run("valid config", func(t *testing.T) {
		cfg := Config{
			Enabled:  []string{"valid-test"},
			Settings: map[string]map[string]interface{}{"valid-test": {"key": "val"}},
		}
		if errs := cfg.Validate(); len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("missing required field", func(t *testing.T) {
		cfg := Config{
			Enabled:  []string{"valid-test"},
			Settings: map[string]map[string]interface{}{"valid-test": {}},
		}
		errs := cfg.Validate()
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
		}
	})

	t.Run("unregistered notifier", func(t *testing.T) {
		cfg := Config{Enabled: []string{"ghost-notifier"}}
		errs := cfg.Validate()
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
		}
	})
}

// ── Retry logic ────────────────────────────────────────────────────────────────

func TestMultiNotifierRetry(t *testing.T) {
	mock := &mockNotifier{name: "retry-test"}
	mock.failUntil.Store(2) // fail first 2 calls

	multi := &MultiNotifier{
		notifiers: []Notifier{mock},
		retry: RetryConfig{
			MaxAttempts: 3,
			BaseDelay:   1 * time.Millisecond,
		},
		metrics: NewMetrics(),
	}

	err := multi.Notify(context.Background(), Notification{Message: "test"})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if mock.calls.Load() != 3 {
		t.Fatalf("expected 3 calls (2 failures + 1 success), got %d", mock.calls.Load())
	}

	snaps := multi.metrics.Snapshots()
	if len(snaps) != 1 {
		t.Fatalf("expected 1 channel in metrics, got %d", len(snaps))
	}
	if snaps[0].Sent != 1 {
		t.Errorf("expected 1 sent, got %d", snaps[0].Sent)
	}
	if snaps[0].Retried != 2 {
		t.Errorf("expected 2 retries, got %d", snaps[0].Retried)
	}
}

func TestMultiNotifierRetryExhausted(t *testing.T) {
	mock := &mockNotifier{name: "exhaust-test"}
	mock.failUntil.Store(10)

	multi := &MultiNotifier{
		notifiers: []Notifier{mock},
		retry: RetryConfig{
			MaxAttempts: 3,
			BaseDelay:   1 * time.Millisecond,
		},
		metrics: NewMetrics(),
	}

	err := multi.Notify(context.Background(), Notification{Message: "test"})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}

	snaps := multi.metrics.Snapshots()
	if snaps[0].Failed != 1 {
		t.Errorf("expected 1 failed, got %d", snaps[0].Failed)
	}
}

// ── Ping ───────────────────────────────────────────────────────────────────────

func TestMultiNotifierPing(t *testing.T) {
	t.Run("all healthy", func(t *testing.T) {
		multi := &MultiNotifier{
			notifiers: []Notifier{
				&mockNotifier{name: "a"},
				&mockNotifier{name: "b"},
			},
			retry:   DefaultRetryConfig(),
			metrics: NewMetrics(),
		}
		if err := multi.Ping(context.Background()); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("one unhealthy", func(t *testing.T) {
		multi := &MultiNotifier{
			notifiers: []Notifier{
				&mockNotifier{name: "good"},
				&mockNotifier{name: "bad", pingErr: fmt.Errorf("connection refused")},
			},
			retry:   DefaultRetryConfig(),
			metrics: NewMetrics(),
		}
		err := multi.Ping(context.Background())
		if err == nil {
			t.Fatal("expected ping error")
		}
	})
}

// ── isRetryable ────────────────────────────────────────────────────────────────

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		err    string
		expect bool
	}{
		{"error status: 503", true},
		{"error status: 429", true},
		{"error status: 500", true},
		{"connection refused", true},
		{"timeout exceeded", true},
		{"discord: webhook_url is required", false},
		{"invalid token", false},
		{"error status: 401", false},
	}
	for _, tt := range tests {
		got := isRetryable(fmt.Errorf("%s", tt.err))
		if got != tt.expect {
			t.Errorf("isRetryable(%q) = %v, want %v", tt.err, got, tt.expect)
		}
	}
}

// ── Metrics ────────────────────────────────────────────────────────────────────

func TestMetricsConcurrency(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.RecordSent("slack")
			m.RecordFailed("discord")
			m.RecordRetried("slack")
		}()
	}
	wg.Wait()

	snaps := m.Snapshots()
	found := map[string]Snapshot{}
	for _, s := range snaps {
		found[s.Name] = s
	}

	if found["slack"].Sent != 100 {
		t.Errorf("slack sent: got %d, want 100", found["slack"].Sent)
	}
	if found["slack"].Retried != 100 {
		t.Errorf("slack retried: got %d, want 100", found["slack"].Retried)
	}
	if found["discord"].Failed != 100 {
		t.Errorf("discord failed: got %d, want 100", found["discord"].Failed)
	}
}

// ── Dispatcher ─────────────────────────────────────────────────────────────────

func TestDispatcherAsync(t *testing.T) {
	mock := &mockNotifier{name: "async-test"}
	d := NewDispatcher(mock, 10)

	for i := 0; i < 5; i++ {
		d.Send(context.Background(), Notification{
			Repo:    "org/repo",
			Message: fmt.Sprintf("msg-%d", i),
		})
	}

	d.Close()
	// Wait a moment for drain goroutine to finish.
	time.Sleep(50 * time.Millisecond)

	if mock.calls.Load() != 5 {
		t.Fatalf("expected 5 calls, got %d", mock.calls.Load())
	}
}

// ── Template resolution ────────────────────────────────────────────────────────

func TestResolveTemplateBasic(t *testing.T) {
	n := Notification{
		Repo:   "org/repo",
		Event:  "push",
		State:  "success",
		Sender: "alice",
	}

	result := ResolveTemplate("Build {{.State}} for {{.Repo}} by {{.Sender}}", n)
	expected := "Build success for org/repo by alice"
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestResolveTemplateInvalid(t *testing.T) {
	n := Notification{Repo: "test"}
	result := ResolveTemplate("{{.Invalid", n)
	if result != "{{.Invalid" {
		t.Errorf("expected raw string fallback, got %q", result)
	}
}

func TestResolveTemplatePayloadField(t *testing.T) {
	payload := map[string]interface{}{
		"pull_request": map[string]interface{}{
			"title":  "Fix bug",
			"number": float64(42),
		},
		"commits": []interface{}{
			map[string]interface{}{"message": "first commit"},
			map[string]interface{}{"message": "second commit"},
		},
	}
	raw, _ := json.Marshal(payload)

	n := Notification{
		Repo:       "org/repo",
		RawPayload: raw,
	}

	tests := []struct {
		tmpl   string
		expect string
	}{
		{`{{.PayloadField "pull_request.title"}}`, "Fix bug"},
		{`PR #{{.PayloadField "pull_request.number"}}`, "PR #42"},
		{`{{.PayloadField "commits.0.message"}}`, "first commit"},
		{`{{.PayloadField "commits.1.message"}}`, "second commit"},
		{`{{.PayloadField "nonexistent.path"}}`, ""},
		{`{{.PayloadField "commits.99.message"}}`, ""},
	}

	for _, tt := range tests {
		result := ResolveTemplate(tt.tmpl, n)
		if result != tt.expect {
			t.Errorf("ResolveTemplate(%q) = %q, want %q", tt.tmpl, result, tt.expect)
		}
	}
}

func TestResolveTemplateNoPayload(t *testing.T) {
	n := Notification{Repo: "org/repo"}
	result := ResolveTemplate(`{{.PayloadField "anything"}}`, n)
	if result != "" {
		t.Errorf("expected empty string for nil payload, got %q", result)
	}
}

// ── repoSlug ───────────────────────────────────────────────────────────────────

func TestRepoSlug(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"org/repo", "org-repo"},
		{"ORG/Repo", "org-repo"},
		{"simple", "simple"},
		{"a/b/c", "a-b-c"},
	}
	for _, tt := range tests {
		got := repoSlug(tt.input)
		if got != tt.expect {
			t.Errorf("repoSlug(%q) = %q, want %q", tt.input, got, tt.expect)
		}
	}
}
