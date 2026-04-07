# Developing Notification Plugins for Eventic

Eventic supports a modular, plugin-based notification system. This document explains how to develop and contribute a new notification plugin (e.g., Email, Twitter, PagerDuty).

## Architecture Overview

The notification system is built on three core concepts:

1. **`Notifier` Interface**: Defines the contract for sending a notification.
2. **Registry**: A global store for notification factories, allowing "plug-and-play" registration.
3. **Multi-Channel Broadcasting**: The client can send a single event to multiple configured notifiers simultaneously.

---

## 1. The Notifier Interface

To create a new notifier, you must implement the `Notifier` interface defined in `client/notifier/notifier.go`:

```go
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
	RawPayload json.RawMessage // full GitHub webhook payload
}

type Notifier interface {
	Notify(ctx context.Context, n Notification) error
	Ping(ctx context.Context) error // health check called at startup
	Name() string
	GetMetrics() *Metrics
}
```

---

## 2. Implementing a Plugin

Create a new file in `client/notifier/yourplugin.go`. 

### Step A: Define your Config and Struct

```go
package notifier

import (
	"context"
	"fmt"
)

type MyPluginNotifier struct {
	APIKey string
}

func (n *MyPluginNotifier) Name() string                      { return "myplugin" }
func (n *MyPluginNotifier) Ping(ctx context.Context) error    { return nil } // validate API key
func (n *MyPluginNotifier) GetMetrics() *Metrics              { return nil }
```

### Step B: Implement `Notify`

Use the `ResolveTemplate` helper to allow users to customize their messages via Go templates.

```go
func (n *MyPluginNotifier) Notify(ctx context.Context, notification Notification) error {
	// 1. Resolve the message template
	message := ResolveTemplate(notification.Message, notification)

	// 2. Perform your API call logic here...
	fmt.Printf("Sending notification to MyPlugin: %s\n", message)
	
	return nil
}
```

### Step C: Register the Plugin

Use an `init()` function to register your plugin factory. This allows the plugin to be used just by importing the package.

```go
func init() {
	Register("myplugin", func(cfg map[string]interface{}) (Notifier, error) {
		apiKey, _ := cfg["api_key"].(string)
		if apiKey == "" {
			return nil, fmt.Errorf("myplugin: api_key is required")
		}
		return &MyPluginNotifier{APIKey: apiKey}, nil
	})
}
```

---

## 3. Configuration

Users enable your plugin in their `config.yaml`:

```yaml
notifier:
  enabled:
    - myplugin
    - slack
  settings:
    myplugin:
      api_key: "your-api-key"
    slack:
      webhook_url: "..."
```

---

## 4. Metadata & Templating

All notifications have access to the following fields in their `Message` template:

| Field | Description |
|---|---|
| `{{.Repo}}` | Repository name (org/repo) |
| `{{.Event}}` | GitHub event type (push, pull_request, etc.) |
| `{{.Action}}` | Action (opened, synchronize, etc.) |
| `{{.HookName}}` | The label of the hook that triggered the notification |
| `{{.Message}}` | The raw message string from the hook config |
| `{{.Stdout}}` | The standard output of the executed hook |
| `{{.Sender}}` | The GitHub user who triggered the event |
| `{{.State}}` | Execution state (success or failure) |
| `{{.PayloadField "key.path"}}` | Extract any field from the raw GitHub webhook payload |

**Example templates:**
```
notify: "Build {{.State}} for {{.Repo}} by {{.Sender}}"
notify: "PR {{.PayloadField \"pull_request.title\"}} needs review"
notify: "Commit: {{.PayloadField \"commits.0.message\"}}"
```

---

## Contribution Checklist

1. [ ] Implement the `Notifier` interface in a new file in `client/notifier/`.
2. [ ] Use `init()` and `Register()` for zero-config integration.
3. [ ] Use `ResolveTemplate()` for the message body.
4. [ ] Handle sensitive configuration via environment variables where appropriate (see `discord.go` for an example using `os.Getenv`).
5. [ ] Add a build check to ensure your plugin compiles: `go build ./client/...`.
