# Eventic

Eventic was born out of pure frustration. Every GitOps tool out there assumes you're running Kubernetes in the cloud with a fancy service mesh, an ingress controller, and a fleet of managed runners. But what if you're not? What if your builds run on bare-metal servers in a closet? What if your deployment target is a VM behind a firewall that GitHub Actions will never reach?

There is no simple, lightweight solution for triggering on-prem or local builds and arbitrary automation based on GitHub webhooks — so we built one.

Eventic is a minimal, two-component system that bridges GitHub webhooks to any machine that can make an outbound WebSocket connection. No inbound ports required on your local network. No complex infrastructure. Just a relay server and a lightweight client.

## Architecture

```
GitHub ──webhook──▶ [Eventic Server] ◀──websocket──▶ [Eventic Client] ──▶ run hooks
              (public / cloud)                    (on-prem / local)
```

1. **Server** — A lightweight relay that receives GitHub webhooks, validates signatures, and fans out events to connected clients over WebSocket.
2. **Client** — A small daemon that connects to the server, listens for events matching its subscriptions, checks out the relevant repo/ref, and executes hooks defined in `.eventic.yaml`.

---

## Server

The server is a single static binary packaged as a Docker container. It exposes three endpoints:

| Endpoint | Method | Purpose |
|---|---|---|
| `/webhook/github` | POST | Receives GitHub webhooks (HMAC-SHA256 validated) |
| `/ws` | GET | WebSocket endpoint for client connections |
| `/healthz` | GET | Health check |

### Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `EVENTIC_WEBHOOK_SECRET` | Yes | — | HMAC secret configured in your GitHub webhook |
| `EVENTIC_CLIENT_TOKENS` | Yes | — | Comma-separated list of tokens that clients use to authenticate |
| `EVENTIC_LISTEN_ADDR` | No | `:8080` | Address and port to listen on |

### Running with Docker

```bash
docker run -d \
  --name eventic-server \
  -p 8080:8080 \
  -e EVENTIC_WEBHOOK_SECRET="your-github-webhook-secret" \
  -e EVENTIC_CLIENT_TOKENS="token1,token2" \
  nitecon/eventic:latest
```

### Docker Compose

```yaml
services:
  eventic:
    image: nitecon/eventic:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      EVENTIC_WEBHOOK_SECRET: "your-github-webhook-secret"
      EVENTIC_CLIENT_TOKENS: "token1,token2"
```

### Deploying to Google Cloud Run

The repository includes a `cloudbuild.yaml` that builds the Docker image and deploys it to Cloud Run automatically. To set this up:

1. **Create a GCP project** (or use an existing one) and enable the following APIs:
   - Cloud Build
   - Cloud Run
   - Container Registry (or Artifact Registry)

2. **Create a Cloud Run service** named `eventic-git` (or update the service name in `cloudbuild.yaml`) and configure the environment variables:

   | Variable | Value |
   |---|---|
   | `EVENTIC_WEBHOOK_SECRET` | Your GitHub webhook HMAC secret |
   | `EVENTIC_CLIENT_TOKENS` | Comma-separated client auth tokens |

   You can set these via the Cloud Run console under **Edit & Deploy New Revision > Variables & Secrets**, or with the CLI:

   ```bash
   gcloud run services update eventic-git \
     --region=us-east4 \
     --set-env-vars="EVENTIC_WEBHOOK_SECRET=your-secret,EVENTIC_CLIENT_TOKENS=token1,token2"
   ```

3. **Set up a Cloud Build trigger** to build and deploy on push:
   - Go to **Cloud Build > Triggers > Create Trigger**
   - Connect your GitHub repository
   - Set the trigger to use the existing `cloudbuild.yaml` at the repo root
   - Choose the branch pattern to trigger on (e.g., `^main$`)

4. **Enable connectivity** — Cloud Run services are publicly accessible by default, which is required so GitHub can deliver webhooks and clients can connect via WebSocket. Ensure:
   - The service allows **unauthenticated invocations** (set under the Cloud Run service's **Security** tab)
   - The Cloud Run service URL is used as your GitHub webhook payload URL (e.g., `https://eventic-git-xxxxx-ue.a.run.app/webhook/github`)

Once the trigger is configured, every push to your selected branch will automatically build and deploy the latest Eventic server to Cloud Run.

### GitHub Webhook Setup

1. Go to your repository (or organization) **Settings > Webhooks > Add webhook**
2. Set **Payload URL** to `https://your-server:8080/webhook/github`
3. Set **Content type** to `application/json`
4. Set **Secret** to the same value as `EVENTIC_WEBHOOK_SECRET`
5. Select the events you want to receive (or choose "Send me everything")

### Bulk Webhook Setup

If you have many repositories, the included `setup-eventic-webhooks.sh` script can add the Eventic webhook to all of them in one pass. It uses the GitHub CLI (`gh`) to iterate over your repos and create the webhook wherever it doesn't already exist.

```bash
# Preview what would be created (no changes made)
./setup-eventic-webhooks.sh \
  --url https://your-server/webhook/github \
  --secret your-webhook-secret \
  --dry-run

# Apply to all repos for the authenticated user and their orgs
./setup-eventic-webhooks.sh \
  --url https://your-server/webhook/github \
  --secret your-webhook-secret

# Target specific owners only
./setup-eventic-webhooks.sh \
  --url https://your-server/webhook/github \
  --secret your-webhook-secret \
  --owner myuser \
  --owner my-org
```

| Flag | Required | Description |
|---|---|---|
| `--url` | Yes | Your Eventic server's webhook endpoint |
| `--secret` | Yes | HMAC secret (must match `EVENTIC_WEBHOOK_SECRET`) |
| `--owner` | No | GitHub user or org to process (repeatable). When omitted, auto-detects the authenticated user and all their organisations |
| `--dry-run` | No | Show what would be created without making changes |

> **Requirements:** [GitHub CLI](https://cli.github.com/) (`gh`) authenticated with admin scope on the target repos, and `jq`.

---

## Client

The client is a small, self-contained binary that runs as a systemd service on any Linux machine. It maintains a persistent WebSocket connection to the server, automatically reconnects with exponential backoff, and executes hooks when events arrive.

### Quick Install

The install script creates the `eventic` user, downloads the binary, installs the systemd service, and drops a template config:

```bash
curl -fsSL https://raw.githubusercontent.com/nitecon/eventic/refs/heads/main/install.sh | sudo bash
```

Then edit `/etc/eventic/config.yaml` with your relay URL, token, and subscriptions, and start the service:

```bash
sudo systemctl start eventic
```

### Manual Installation

Download the latest release for your platform from the [Releases page](https://github.com/nitecon/eventic/releases):

```bash
# Download to temp, then install into /opt/eventic/bin
curl -fsSL -o /tmp/eventic-client \
  https://github.com/nitecon/eventic/releases/latest/download/eventic-client-linux-amd64

sudo mkdir -p /opt/eventic/bin
sudo mv /tmp/eventic-client /opt/eventic/bin/eventic-client
sudo chmod +x /opt/eventic/bin/eventic-client
sudo chown eventic:eventic /opt/eventic/bin/eventic-client

# Symlink into PATH for convenience
sudo ln -sf /opt/eventic/bin/eventic-client /usr/local/bin/eventic-client
```

### Configuration

Create `/etc/eventic/config.yaml`:

```yaml
relay: "wss://your-server:8080/ws"
token: "your-auth-token"
client_id: "your-client-id"
repos_dir: "/opt/eventic/repos"
auto-update: true
global-ignore-pre:
  - "*"
global-ignore-post:
  - workflow_job.in_progress
  - workflow_job.queued
  - workflow_run.requested
  - workflow_run.in_progress
  - check_run.*
  - release.edited
  - release.published
global-hooks:
  pre: "echo preparing ${EVENTIC_REPO}..."
  post: "claude -p 'Validate the repository at ${EVENTIC_REPOS}/${EVENTIC_REPO} and report any issues.'"
  notify: "Global hook {{.State}} for {{.Repo}}"
  notify_on: [failure]
notifier:
  enabled:
    - discord
  settings:
    discord:
      token: "your-bot-token"
      guild_id: "your-guild-id"
subscribe:
  - "myuser/*"
  - "myworkorg/*"
  - "kubernetes/specific-repo"
```

| Field | Description |
|---|---|
| `relay` | WebSocket URL of the Eventic server |
| `token` | Authentication token (must be listed in the server's `EVENTIC_CLIENT_TOKENS`) |
| `client_id` | Unique identifier for this client |
| `repos_dir` | Directory where repos will be cloned and managed |
| `subscribe` | List of glob patterns to filter which repositories trigger hooks (see [Subscription Patterns](#subscription-patterns)) |
| `auto-update` | When `true`, the client checks for new releases every 5 minutes and updates itself automatically |
| `auto-check` | Verifies repo health every 5 minutes and re-clones broken repos (one at a time). **Defaults to `true`** — set to `false` to disable |
| `max-workers` | Maximum concurrent event processors (defaults to the number of CPUs). Events for the same repo are always serialized |
| `global-hooks.pre` | Fallback pre hook — runs for repos that have no `.eventic.yaml` or `.deploy/deploy.yml` |
| `global-hooks.post` | Fallback post hook — runs for repos that have no `.eventic.yaml` or `.deploy/deploy.yml` |
| `global-hooks.notify` | Notification template for global hooks (supports Go templates, see [Notifications](#notifications)) |
| `global-hooks.notify_on` | When to send global hook notifications: `[failure]`, `[success]`, or `[success, failure]`. Omit to always notify |
| `notifier` | Notification channel configuration block (see [Notifications](#notifications)) |
| `global-ignore-pre` | List of event patterns to skip global pre hook execution (see [Global Ignore Patterns](#global-ignore-patterns)) |
| `global-ignore-post` | List of event patterns to skip global post hook execution (see [Global Ignore Patterns](#global-ignore-patterns)) |
| `global-allowed-pre` | Allowlist of event patterns for global pre hook — when non-empty, **only** matching events run the hook and ignore patterns are disregarded (see [Global Allowed Patterns](#global-allowed-patterns)) |
| `global-allowed-post` | Allowlist of event patterns for global post hook — same behaviour as `global-allowed-pre` but for the post hook |

### Subscription Patterns

Subscriptions use Go's `path.Match` glob syntax to filter which repositories trigger hooks on the client. Repositories arrive as `org/repo`, so patterns must account for the `/` separator — a bare `*` will **not** match across it.

| Pattern | Matches | Use case |
|---|---|---|
| `myuser/*` | All repos under `myuser` | Subscribe to everything in your personal account |
| `myworkorg/*` | All repos under `myworkorg` | Subscribe to everything in your work organization |
| `kubernetes/kops` | Exactly `kubernetes/kops` | Subscribe to a specific repo you contribute to |
| `myorg/infra-*` | `myorg/infra-web`, `myorg/infra-db`, etc. | Subscribe to repos matching a naming convention |
| `*/*` | Every org and every repo | Subscribe to absolutely everything (see note below) |

A typical setup combines broad org-level wildcards for accounts you own with pinpoint subscriptions for external repos you contribute to:

```yaml
subscribe:
  - "myuser/*"            # all personal repos
  - "myworkorg/*"         # all work org repos
  - "kubernetes/kops"     # specific external repo I contribute to
  - "prometheus/node_*"   # external repos matching a prefix
```

> **Note:** `*/*` subscribes to every event the server receives. This only makes sense if your server's webhook is scoped to repos you care about. The only requirement for any pattern to work is that the corresponding GitHub webhook has been added to the repository or organization — Eventic can only relay events it actually receives.

### Global Ignore Patterns

The `global-ignore-pre` and `global-ignore-post` lists let you skip global hook execution for specific event types. This is useful for filtering out noisy events (like `check_run` or `workflow_run`) that would otherwise trigger your global hooks unnecessarily.

**These only affect global hooks** — event-specific hooks defined in `.eventic.yaml` are never skipped by ignore patterns.

| Pattern | Matches | Example |
|---|---|---|
| `check_run.completed` | Exact event type and action | Only `check_run` events with action `completed` |
| `check_run.*` | Any action for the event type | All `check_run` events regardless of action |
| `check_run` | All actions (same as `check_run.*`) | All `check_run` events regardless of action |
| `"*.completed"` | Any event with a specific action | Any event type where the action is `completed` |
| `"*"` | Everything | All events |

> **YAML quoting:** Patterns that start with `*` must be quoted (`"*"`, `"*.completed"`, `"*.*"`) because YAML treats a bare `*` as an anchor alias. Patterns where `*` appears after other characters (like `check_run.*`) do not need quoting.

```yaml
global-ignore-pre:
  - check_run.completed      # skip pre hook for completed check runs
  - workflow_run.*            # skip pre hook for all workflow_run events
  - deployment_status         # skip pre hook for all deployment_status events
  - "*.completed"             # skip pre hook for any event with action completed
  - "*"                       # skip pre hook for everything (must be quoted)
global-ignore-post:
  - check_run                 # skip post hook for all check_run events
```

> **Tip:** For a full list of GitHub webhook event types and their actions, see `client/github_events.go` in the source or the [GitHub webhook documentation](https://docs.github.com/en/webhooks/webhook-events-and-payloads).

### Global Allowed Patterns

The `global-allowed-pre` and `global-allowed-post` lists act as an **allowlist** for global hooks. When a list has one or more entries, **only** events matching at least one pattern will trigger the corresponding global hook — everything else is silently skipped and the ignore list is completely disregarded.

This is the inverse of the ignore lists: instead of "run for everything except these", it's "run for **only** these". Use it when you want a single event (or a small set) to be the only trigger for your global hooks.

**These only affect global hooks** — event-specific hooks defined in `.eventic.yaml` are never filtered by allowed patterns.

The pattern syntax is identical to [Global Ignore Patterns](#global-ignore-patterns) (remember to quote patterns starting with `*`):

```yaml
# Only run global hooks for workflow_job.completed — ignore everything else
global-allowed-pre:
  - workflow_job.completed
global-allowed-post:
  - workflow_job.completed
  - push                       # also run post hook on push events
  - "*.created"                # also run post hook for any event with action created
```

> **Precedence:** When `global-allowed-pre` (or `-post`) is non-empty, the corresponding `global-ignore-pre` (or `-post`) list is ignored entirely. If the allowed list is empty (or omitted), the ignore list applies as usual.

### Systemd Service

Create `/etc/systemd/system/eventic.service`:

```ini
[Unit]
Description=Eventic Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/eventic-client -c /etc/eventic/config.yaml
Restart=always
RestartSec=5
User=eventic
Group=eventic
WorkingDirectory=/opt/eventic

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo useradd -r -s /bin/false eventic
sudo mkdir -p /opt/eventic/{bin,repos} /etc/eventic
sudo chown -R eventic:eventic /opt/eventic
sudo systemctl daemon-reload
sudo systemctl enable --now eventic
```

### Auto-Update

When `auto-update: true` is set in the client config, the client will check GitHub releases every 5 minutes for a newer version. If one is found, it downloads the matching binary for the current OS and architecture, resolves any symlinks to find the real binary location (e.g. `/opt/eventic/bin/eventic-client`), replaces itself, and exits. The systemd service (configured with `Restart=always`) then restarts automatically with the new binary.

Because the binary lives in `/opt/eventic/bin/` (owned by the `eventic` user), the auto-updater can write updates without requiring root privileges. The symlink in `/usr/local/bin/` continues to point to the updated binary automatically.

Source builds (without a release version) will update to the latest published release on the first check.

---

## Hook Configuration

Hooks are defined per-repository in `.eventic.yaml` at the repo root:

```yaml
hooks:
  pre: "echo preparing..."
  post: "echo done"
events:
  push:
    post: "make deploy"
  pull_request:
    post: "make lint"
  pull_request.opened:
    post: "claude -p 'Review this PR' --headless"
  pull_request.synchronize:
    post: "make test"
  release.published:
    post: "/opt/scripts/notify-release.sh"
  issues.opened:
    post: "claude -p 'Triage this issue' --headless"
```

### Execution Order

For each event the client receives:

1. **Global pre hook** (`hooks.pre`) — runs before anything else
2. **Event-specific pre hook** (`events.<event>.pre`) — runs before checkout
3. **Git checkout** — switches to the correct ref/branch/PR
4. **Event-specific post hook** (`events.<event>.post`) — runs after checkout
5. **Global post hook** (`hooks.post`) — runs after everything

### Event Matching

Hooks are matched with action specificity: `pull_request.opened` takes precedence over `pull_request`. If no action-specific hook exists, the event-level hook is used as a fallback.

### Environment Variables

Every hook receives these environment variables:

| Variable | Description |
|---|---|
| `EVENTIC_REPO` | Full repository name (e.g., `org/repo`) |
| `EVENTIC_REF` | Git ref (branch name, tag, or PR ref) |
| `EVENTIC_EVENT` | GitHub event type (e.g., `push`, `pull_request`) |
| `EVENTIC_ACTION` | Event action (e.g., `opened`, `synchronize`) |
| `EVENTIC_SENDER` | GitHub username that triggered the event |
| `EVENTIC_PR_NUMBER` | Pull request number (0 if not a PR event) |
| `EVENTIC_DELIVERY_ID` | Unique GitHub delivery ID for tracing |

### Hook Resolution Order

The client resolves hooks using the following precedence (first match wins):

1. **`.eventic.yaml`** in the repository root — full per-repo hook configuration
2. **`.deploy/deploy.yml`** in the repository — used as a [Bruce](https://github.com/nitecon/bruce) manifest for push events
3. **Client-level global hooks** (`global-hooks.pre` / `global-hooks.post` in the client config) — fallback for repos with no hook configuration of their own

This means you can set up default automation for all subscribed repos by adding a `global-hooks:` block to your client config, and individual repos can override it by adding their own `.eventic.yaml`.

### Approval System

Eventic supports an optional approval gate that blocks events from unapproved repositories or senders. To enable it, add `require_approval: true` to your `config.yaml`:

```yaml
require_approval: true
approvals_path: "/etc/eventic/approvals.json"  # optional, this is the default
```

When enabled, blocked events trigger a notification with the required approval command.

```bash
# Approve a specific repository
eventic-client approve --repo "org/repo"

# Approve a specific GitHub user
eventic-client approve --sender "username"

# Revoke approval
eventic-client revoke --repo "org/repo"
eventic-client revoke --sender "username"

# List all current approvals
eventic-client list-approvals
```

Approvals are persisted in `/etc/eventic/approvals.json` (configurable via `approvals_path` in `config.yaml`).

---

## Notifications

Eventic supports multiple notification channels (Discord, Slack, etc.) that can be triggered by global or per-repo hooks. Notifications are a two-step process: **configure a channel** in your client config, then **add `notify` fields** to your hooks.

### Quick Start: Discord Webhook

The fastest way to get notifications working is with a Discord webhook:

1. **Create a Discord webhook** — In your Discord server, go to **Server Settings > Integrations > Webhooks > New Webhook**. Choose the target channel, copy the **Webhook URL**.

2. **Add the notifier to your client config** (`/etc/eventic/config.yaml`):

   ```yaml
   notifier:
     enabled:
       - discord_webhook
     settings:
       discord_webhook:
         webhook_url: "https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN"
   ```

3. **Add `notify` to a hook** — either in your client config's global hooks or in a repo's `.eventic.yaml`:

   ```yaml
   # In /etc/eventic/config.yaml (global hooks)
   global-hooks:
     post: "make build"
     notify: "Build {{.State}} for {{.Repo}}"
     notify_on: [failure]
   ```

   ```yaml
   # Or in your repo's .eventic.yaml (per-repo hooks)
   events:
     push:
       post: "make deploy"
       notify: "Deploy {{.State}} for {{.Repo}} by {{.Sender}}"
   ```

4. **Restart the client** — `sudo systemctl restart eventic`. At startup, Eventic will health-check the webhook and log the result.

That's it — the next matching event will send a notification to your Discord channel.

### Quick Start: Discord Bot

For richer embeds and automatic per-repo channel creation, use the Discord bot notifier:

1. **Create a Discord Application** at [discord.com/developers](https://discord.com/developers/applications). Add a Bot, copy the **Token**.
2. **Invite the bot** to your server with the `bot` scope and **Manage Channels** + **Send Messages** permissions.
3. **Add the notifier to your client config** (`/etc/eventic/config.yaml`):

   ```yaml
   notifier:
     enabled:
       - discord
     settings:
       discord:
         token: "your-bot-token"       # or set DISCORD_BOT_TOKEN env var
         guild_id: "your-guild-id"     # or set DISCORD_GUILD_ID env var
         category_id: "1234..."        # optional: auto-created channels go here
   ```

4. **Add `notify` to a hook** and **restart the client** (same as the webhook steps above).

Eventic will automatically create a text channel per repository (e.g., `#org-repo`) inside the specified category. To send all notifications to a single channel instead, use `channel_id` in place of `guild_id`.

### Notification Channels

Add a `notifier` block to `/etc/eventic/config.yaml`. List channel names in `enabled` and provide their settings:

```yaml
notifier:
  enabled:
    - discord
    - discord_webhook
    - slack
  settings:
    discord:
      token: "your-bot-token"       # Can also use DISCORD_BOT_TOKEN env
      guild_id: "your-guild-id"     # Can also use DISCORD_GUILD_ID env
      category_id: "1234..."        # Optional: auto-created channels go here
      channel_id: "5678..."         # Optional: send all messages to one channel
    discord_webhook:
      webhook_url: "https://discord.com/api/webhooks/..."
    slack:
      webhook_url: "https://hooks.slack.com/services/..."
```

| Channel | Required Settings | Description |
|---|---|---|
| `discord` | `token` + (`guild_id` or `channel_id`) | Discord Bot with rich embeds; auto-creates per-repo channels |
| `discord_webhook` | `webhook_url` | Posts to a Discord channel via webhook (simplest setup) |
| `slack` | `webhook_url` | Posts to a Slack channel via incoming webhook |

You can enable multiple channels simultaneously — all enabled channels receive every notification. For example, you can use `discord` for per-repo channels and add `discord_webhook` as an extra notification point to a shared ops channel.

### Adding Notifications to Hooks

Notifications are triggered by adding `notify` (and optionally `notify_on`) fields to hooks. This works in two places:

**Global hooks** in `/etc/eventic/config.yaml`:

```yaml
global-hooks:
  pre: "echo preparing ${EVENTIC_REPO}..."
  post: "make build"
  notify: "Global hook {{.State}} for {{.Repo}}"
  notify_on: [failure]  # only notify on failure
```

**Per-repo hooks** in `.eventic.yaml` at the repository root:

```yaml
hooks:
  pre: "echo starting..."
  notify: "Build started for {{.Repo}}"
  notify_on: [failure]
events:
  push:
    post: "make deploy"
    notify: "Deployment {{.State}}! Output: {{.Stdout}}"
    notify_on: [success, failure]
  pull_request.opened:
    post: "make lint"
    notify: 'PR "{{.PayloadField "pull_request.title"}}" by {{.Sender}}'
    notify_on: [failure]
  release.published:
    post: "/opt/scripts/release.sh"
    notify: "Release {{.PayloadField \"release.tag_name\"}} published for {{.Repo}}"
```

### Notification Filtering

Use `notify_on` to control when notifications fire:

| Value | Behavior |
|---|---|
| *(omitted)* | Always notify (default) |
| `[failure]` | Only notify on hook failure |
| `[success]` | Only notify on success |
| `[success, failure]` | Explicit: notify on both |

### Metadata & Templating

Notifications support Go templates with the following fields:

| Field | Description |
|---|---|
| `{{.Repo}}` | Repository name |
| `{{.Event}}` | GitHub event type |
| `{{.Action}}` | Event action (opened, synchronize, etc.) |
| `{{.State}}` | Execution state (`success` or `failure`) |
| `{{.Stdout}}` | Output of the executed hook |
| `{{.Sender}}` | GitHub user who triggered the event |
| `{{.PayloadField "key.path"}}` | Extract any field from the raw GitHub webhook payload |

**Payload field examples:**
- `{{.PayloadField "pull_request.title"}}` — PR title
- `{{.PayloadField "commits.0.message"}}` — First commit message
- `{{.PayloadField "head_commit.author.name"}}` — Push author name

---

## Developing Plugins

Eventic is designed to be easily extensible. To learn how to build your own notification plugin (e.g., for Email, Telegram, or internal APIs), see [docs/Notifications.md](docs/Notifications.md).

---

## Building from Source

```bash
# Server
CGO_ENABLED=0 go build -ldflags="-s -w" -o eventic-server ./server/cmd/

# Client
CGO_ENABLED=0 go build -ldflags="-s -w" -o eventic-client ./client/cmd/
```

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
