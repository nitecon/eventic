# Eventic

Eventic was born out of pure frustration. Every GitOps tool out there assumes you're running Kubernetes in the cloud with a fancy service mesh, an ingress controller, and a fleet of managed runners. But what if you're not? What if your automation runs on bare-metal servers in a closet? What if your target is a VM behind a firewall that GitHub Actions will never reach?

There is no simple, lightweight solution for triggering on-prem or local automation based on GitHub webhooks — so we built one.

Eventic is a minimal, two-component system that bridges GitHub webhooks (and free-form agent "comms" messages) to any machine that can make an outbound WebSocket connection. No inbound ports required on your local network. No complex infrastructure. Just a relay server and a lightweight client.

It is a **per-repo workflow engine**: when an event arrives, the client resolves a **workflow** for that repository and event type, checks out the branch, and runs a **DAG of typed action steps** with output and context flowing between nodes. Actions can run commands, send HTTP requests, send project messages, trigger project events, evaluate responses, and run post-actions over response data. Workflows are authored from the dashboard/API, not from in-repo YAML.

## Architecture

```
GitHub ──webhook──────┐
                      ├─▶ [Eventic Server] ◀──websocket──▶ [Eventic Client] ──▶ resolve workflow ─▶ run DAG
POST /event/comms ────┘     (public / cloud)                  (on-prem / local)
```

1. **Server** — A lightweight relay that receives GitHub webhooks (HMAC-validated) and authenticated `comms` messages, then fans events out to connected clients over WebSocket.
2. **Client** — A small daemon that connects to the server, listens for events matching its subscriptions, resolves a workflow from its local SQLite database (repo overrides global, per event type), checks out the relevant repo/ref, and executes the workflow DAG.

---

## Server

The server is a single static binary packaged as a Docker container. It exposes these endpoints:

| Endpoint | Method | Purpose |
|---|---|---|
| `/webhook/github` | POST | Receives GitHub webhooks (HMAC-SHA256 validated) |
| `/event/comms` | POST | Injects a free-form agent `comms` event (Bearer-token authenticated) |
| `/ws` | GET | WebSocket endpoint for client connections |
| `/healthz` | GET | Health check |

### Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `EVENTIC_WEBHOOK_SECRET` | Yes | — | HMAC secret configured in your GitHub webhook |
| `EVENTIC_CLIENT_TOKENS` | Yes | — | Comma-separated list of tokens that clients use to authenticate |
| `EVENTIC_COMMS_TOKENS` | No | falls back to `EVENTIC_CLIENT_TOKENS` | Comma-separated Bearer tokens accepted on `POST /event/comms` |
| `EVENTIC_LISTEN_ADDR` | No | `:8080` | Address and port to listen on |

### The `comms` Event

`POST /event/comms` lets an external agent inject a synthetic event into the same relay path GitHub webhooks use. Authenticate with `Authorization: Bearer <token>` (checked against `EVENTIC_COMMS_TOKENS`, or `EVENTIC_CLIENT_TOKENS` if that is unset). The body is JSON:

```bash
curl -X POST https://your-server:8080/event/comms \
  -H "Authorization: Bearer your-comms-token" \
  -H "Content-Type: application/json" \
  -d '{
        "repo": "myorg/myrepo",
        "ref": "main",
        "message": "Assess the latest dependency bump for supply-chain risk",
        "sender": "security-agent"
      }'
```

| Field | Required | Description |
|---|---|---|
| `repo` | Yes | `org/repo` the workflow runs against |
| `message` | Yes | Free-form payload exposed to workflow nodes as `EVENTIC_MESSAGE` |
| `ref` | No | Branch/ref to check out (defaults to the repo's default branch) |
| `sender` | No | Identifier recorded as the event sender |
| `clone_url` | No | Override clone URL (defaults to `https://github.com/<repo>.git`) |

The server builds an `EventMsg` with `GitHubEvent: "comms"` and pushes it onto the same channel as webhooks. On the client, the resolved `comms` workflow runs with the message available to every node via `EVENTIC_MESSAGE`.

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

The client is a small, self-contained binary that runs as a systemd service on any Linux machine. It maintains a persistent WebSocket connection to the server, automatically reconnects with exponential backoff, and runs the resolved workflow DAG when events arrive.

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
max-node-workers: 1   # concurrent nodes per run; 1 = serialize (safe default)
default-notify: "Workflow {{.State}} for {{.Repo}} ({{.Event}}.{{.Action}})"
default-notify-on: [failure]
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
web:
  enabled: true
  listen: "127.0.0.1:16384"
  static_dir: "/opt/eventic/dashboard"   # optional: serve a custom ndesign UI
  token: "dashboard-ws-token"            # optional: gate the live /ws/runs stream
state:
  enabled: true
  path: "/opt/eventic/state/eventic.db"
```

> **Workflows are not configured in YAML.** They live in the client's SQLite database and are authored through the dashboard / [Workflow API](#workflow-api). The client config only covers connectivity, the notifier, the local console, and executor knobs. See [docs/Workflows.md](docs/Workflows.md) for the workflow/DAG model.

| Field | Description |
|---|---|
| `relay` | WebSocket URL of the Eventic server |
| `token` | Authentication token (must be listed in the server's `EVENTIC_CLIENT_TOKENS`) |
| `client_id` | Unique identifier for this client |
| `repos_dir` | Directory where repos will be cloned and managed |
| `subscribe` | List of glob patterns to filter which repositories trigger workflows (see [Subscription Patterns](#subscription-patterns)) |
| `auto-update` | When `true`, the client checks for new releases every 5 minutes and updates itself automatically |
| `auto-check` | Verifies repo health every 5 minutes and re-clones broken repos (one at a time). **Defaults to `true`** — set to `false` to disable |
| `max-workers` | Maximum concurrent event processors (defaults to the number of CPUs). Events for the same repo are always serialized |
| `max-node-workers` | Maximum nodes executed concurrently **within a single workflow run** (defaults to `1`). Nodes share one checkout, so keep this at `1` unless your workflow avoids working-tree races (see [docs/Workflows.md](docs/Workflows.md)) |
| `notifier` | Notification channel configuration block (see [Notifications](#notifications)) |
| `default-notify` | Notification template fired at **workflow-run completion** (success/failure). Supports Go templates (see [Notifications](#notifications)) |
| `default-notify-on` | `notify_on` filter applied to `default-notify` (e.g., `[failure]`). Only used when `default-notify` is set |
| `web.enabled` | Enables the client-local web console + JSON/WS API. Defaults to `false` |
| `web.listen` | Address for the local web console. Defaults to `127.0.0.1:16384` when enabled |
| `web.static_dir` | Optional directory containing a custom ndesign dashboard (`index.html` served at `/`). When empty, a minimal embedded shell is served |
| `web.token` | Optional token gating the live `/ws/runs` stream — clients must pass a matching `?token=`. When empty the stream is open (localhost default) |
| `web.max_events` | Number of recent client executions retained in memory for replay/API startup. Defaults to `100` |
| `web.max_output_bytes` | Maximum retained output bytes per node/description. Defaults to `65536` |
| `state.enabled` | Enables the client-local persistent SQLite database (projects, workflows, and runs). Defaults to `false` |
| `state.path` | SQLite database path. Defaults to `/opt/eventic/state/eventic.db` when enabled |

> **Removed keys.** The `global-hooks`, `global-ignore-pre`/`global-ignore-post`, and `global-allowed-pre`/`global-allowed-post` keys from earlier hook-pipeline releases no longer exist. Global automation is now a `global`-scope workflow; per-event filtering is expressed as workflow resolution (a repo/event with no workflow simply runs nothing).

### Client State and Web Console

Persistent project state and the web server are both opt-in. If `state.enabled` is not true, no project state is written to disk. If `web.enabled` is not true, the client does not start an HTTP listener.

To persist the latest managed-project state to SQLite without exposing an HTTP endpoint:

```yaml
state:
  enabled: true
  path: "/opt/eventic/state/eventic.db"
```

To also expose the local web/API server:

```yaml
web:
  enabled: true
  listen: "127.0.0.1:16384"
  max_events: 100
  max_output_bytes: 65536

state:
  enabled: true
  path: "/opt/eventic/state/eventic.db"
```

When `web.enabled` is true, the client starts a local-only web console + API on `web.listen`. This runs on the Eventic client, not the public relay server, so node output and run logs stay on the machine that executed them. The UI is an [ndesign](https://github.com/nitecon/ndesign)-driven dashboard for authoring workflows and watching runs live; the backend only guarantees the JSON/WS [Workflow API](#workflow-api) and the `<meta name="endpoint:*">` wiring. Drop your own dashboard build into `web.static_dir` to override the embedded shell.

On startup, the client scans `repos_dir` and adds every checked-out repository it finds to the project inventory, so `/api/projects` includes repos that exist on disk even before any workflow has run. Repositories discovered later from webhook or `comms` events are added immediately and updated with their checked-out git state after clone/checkout completes.

The console serves the [Workflow API](#workflow-api) plus a small set of legacy/local helpers:

| Endpoint | Purpose |
|---|---|
| `/` | ndesign dashboard shell (custom build via `web.static_dir`, else embedded) |
| `/api/...` | JSON workflow-authoring + run API (see [Workflow API](#workflow-api)) |
| `/ws/runs` | WebSocket live run/node stream |
| `/events` | JSON array of recent executions, newest first |
| `/events/stream` | Server-Sent Events stream for live execution updates |
| `/healthz` | Local health check |

When persistent state is enabled, SQLite stores one current row per managed repository in the `projects` table (repo name, latest event/action/delivery ID, final state, latest combined output, description, checked-out git ref and commit hash, timing fields). Authored workflows live in `workflows` / `workflow_nodes` / `workflow_edges`, and each execution is recorded in `workflow_runs` / `workflow_run_nodes` with bounded per-node output. A compatibility `project_events` history table keeps the last few delivery records per repository.

For Kubernetes clients, mount persistent storage at `/opt/eventic/state` or set `state.path` to a path inside your own volume mount:

```yaml
volumeMounts:
  - name: eventic-state
    mountPath: /opt/eventic/state
volumes:
  - name: eventic-state
    persistentVolumeClaim:
      claimName: eventic-state
```

### Subscription Patterns

Subscriptions use Go's `path.Match` glob syntax to filter which repositories trigger workflows on the client. Repositories arrive as `org/repo`, so patterns must account for the `/` separator — a bare `*` will **not** match across it.

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

> **Event filtering is workflow resolution.** Earlier releases used `global-ignore-*` / `global-allowed-*` lists to gate a global hook. There are no global hooks anymore: an event runs a workflow only if one is resolved for its `(scope, repo, event_type)` slot (repo overrides global). To "ignore" an event, simply don't author a workflow for it; to act on it for every repo, author a `global`-scope workflow. See [Workflows](#workflows) and [docs/Workflows.md](docs/Workflows.md).

> **Tip:** For a full list of GitHub webhook event types and their actions (the values you pick when authoring a workflow), see `client/github_events.go` in the source, the [GitHub webhook documentation](https://docs.github.com/en/webhooks/webhook-events-and-payloads), or `GET /api/event-types` (which also includes `comms`).

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

## Workflows

A **workflow** is a directed acyclic graph (DAG) of typed action **nodes** bound to one `(scope, repo, event_type)` slot. When a matching event arrives, the client checks out the branch and executes the graph, passing output and context between nodes. Workflows are stored in the client's SQLite database and authored via the dashboard / [Workflow API](#workflow-api) — there is no in-repo config file.

For the full model (nodes, edges, conditions, context passing, parallelism, and a worked agentic example) see **[docs/Workflows.md](docs/Workflows.md)**. The essentials:

- **Nodes** use typed actions: `run_command`, `send_http_request`, `send_project_message`, or `trigger_project_event`. Each action has typed config, status/exit-code evaluation, optional response handling (`noop`, `send_data`, `iterate_data`), optional `post_actions`, `capture`, `timeout`, and `continue_on_error`.
- **Edges** are directed and conditional. A node becomes eligible when all its parents are terminal, and runs only if at least one incoming edge condition is satisfied; otherwise it is **skipped** (skips propagate). Supported conditions:

  | Condition | Runs the target when… |
  |---|---|
  | `always` (or empty) | the source node finished (success or failure) |
  | `success` | the source node succeeded |
  | `failure` | the source node failed |
  | `NAME == value` | captured variable `NAME` equals `value` (case-insensitive) |
  | `NAME != value` | captured variable `NAME` does not equal `value` |

  An unrecognized condition **fails closed** (the edge is not satisfied), so a malformed gate never silently fires.

### Node Environment

Every node command receives the standard `EVENTIC_*` environment, the comms message, a per-run scratch directory, and every variable captured by upstream nodes so far (`NAME=value`):

| Variable | Description |
|---|---|
| `EVENTIC_REPOS` | Root directory containing managed repositories (e.g., `/opt/eventic/repos`) |
| `EVENTIC_REPO` | Full repository name (e.g., `org/repo`) |
| `EVENTIC_REF` | Git ref (branch name, tag, or PR ref) |
| `EVENTIC_EVENT` | Event type (`push`, `pull_request`, `comms`, …) |
| `EVENTIC_ACTION` | Event action (e.g., `opened`, `synchronize`) |
| `EVENTIC_SENDER` | Who triggered the event |
| `EVENTIC_MESSAGE` | Free-form payload from a `comms` event (empty for GitHub events) |
| `EVENTIC_CONTEXT_DIR` | Per-run scratch directory shared by all nodes in the run |
| `EVENTIC_PR_NUMBER` | Pull request number (0 if not a PR event) |
| `EVENTIC_DELIVERY_ID` | Unique delivery ID for tracing |
| `EVENTIC_RESPONSE_DATA` | Selected response data while a post action is running |
| `EVENTIC_ITEM` | Current iterated item while an `iterate_data` post action is running |
| `EVENTIC_ITEM_INDEX` | Current zero-based item index while an `iterate_data` post action is running |
| `<captured vars>` | Each upstream `capture` exported as `NAME=value` |

### Workflow Resolution

When an event arrives the client resolves exactly one workflow with **repo overriding global**, most specific first:

1. `repo` scope, `event.action` (e.g. `pull_request.opened`)
2. `repo` scope, `event` (e.g. `pull_request`)
3. `global` scope, `event.action`
4. `global` scope, `event`

The first enabled match wins. If nothing matches, the event is recorded as `skipped` and nothing runs. A repo that defines its own workflow for an event opts **out** of the global workflow for that event (no composition).

## Workflow API

When `web.enabled` is true the client exposes a JSON/WS API for authoring workflows and inspecting runs. All responses are `application/json`; errors use the ndesign envelope `{"errors":{"error":"...","field":"..."}}`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/workflows` | List workflows (filter `?scope=&repo=&event=`) |
| `POST` | `/api/workflows` | Create a workflow (nodes + edges) |
| `GET` | `/api/workflows/{id}` | Fetch one workflow with nodes + edges |
| `PUT` | `/api/workflows/{id}` | Replace a workflow; validates DAG (cycle/reference check) |
| `DELETE` | `/api/workflows/{id}` | Delete a workflow |
| `GET` | `/api/workflow-config?scope=&repo=` | Dashboard-friendly workflow config with typed step projections |
| `POST` | `/api/workflow-config?scope=&repo=` | Create a workflow from typed step fields, `typed_steps`, or legacy newline `steps` |
| `PUT` | `/api/workflow-config/{id}` | Update workflow metadata or replace config-compatible steps |
| `DELETE` | `/api/workflow-config/{id}` | Delete a configured workflow |
| `POST` | `/api/workflow-config/{id}/steps` | Append a typed step to a configured workflow |
| `PUT` | `/api/workflow-config/{id}/steps/{node_key}` | Update one typed step |
| `DELETE` | `/api/workflow-config/{id}/steps/{node_key}` | Delete one typed step |
| `GET` | `/api/event-types` | Reference event list (GitHub events **plus `comms`**) for editor dropdowns |
| `GET` | `/api/projects`, `/api/projects/{repo}` | Managed-project summaries |
| `GET` | `/api/refs?repo=org/repo` | Local branch/tag refs for the repo |
| `GET` | `/api/runs?repo=&workflow=` | List runs |
| `POST` | `/api/runs` | Manually trigger a run `{repo, event_type, ref, message?}` |
| `GET` | `/api/runs/{id}` | Run detail with per-node state/output |
| `GET` | `/api/runs/stream` | Server-Sent Events fallback stream |
| `GET` | `/ws/runs?token=&repo=` | WebSocket live stream; frames carry `type:"run"` / `type:"node"` (no subscribe handshake; `token` required only when `web.token` is set) |

### Example: author a workflow

```bash
curl -X POST http://127.0.0.1:16384/api/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "scope": "repo",
    "repo": "myorg/myrepo",
    "event_type": "pull_request",
    "name": "PR review",
    "enabled": true,
    "nodes": [
      {"node_key": "lint",   "name": "Lint",   "type": "run_command", "command": "make lint"},
      {"node_key": "test",   "name": "Test",   "type": "run_command", "command": "make test"},
      {"node_key": "review", "name": "Review", "type": "run_command",
       "command": "claude -p \"Review the diff for $EVENTIC_REPO\"",
       "continue_on_error": true}
    ],
    "edges": [
      {"from_node": "lint", "to_node": "test",   "condition": "success"},
      {"from_node": "test", "to_node": "review", "condition": "success"}
    ]
  }'
```

### Example: trigger a run manually

```bash
curl -X POST http://127.0.0.1:16384/api/runs \
  -H "Content-Type: application/json" \
  -d '{"repo":"myorg/myrepo","event_type":"pull_request","ref":"feature/x"}'
```

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

Eventic supports multiple notification channels (Discord, Slack, etc.). There are two ways to send a notification:

1. **Run-completion notifications** — set `default-notify` (and optionally `default-notify-on`) in the client config; Eventic fires it automatically when a workflow run finishes.
2. **Per-node notifications** — a workflow node whose command calls the notifier (or any messaging CLI) directly, for fine-grained, mid-run messages.

Either way you first **configure a channel** in your client config.

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

3. **Add run-completion notifications** — set `default-notify` in the same config:

   ```yaml
   # In /etc/eventic/config.yaml — fired when a workflow run finishes
   default-notify: "Workflow {{.State}} for {{.Repo}} ({{.Event}}.{{.Action}})"
   default-notify-on: [failure]   # omit to always notify
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
         notify_channel_id: "5678..."  # optional: route all notifications to this channel ID
   ```

4. **Set `default-notify`** (or add a notifier node to a workflow) and **restart the client** (same as the webhook steps above).

Eventic will automatically create a text channel per repository (e.g., `#org-repo`) inside the specified category. To send all notifications to a single dedicated channel instead, set `notify_channel_id` to the Discord channel ID (right-click the channel in Discord with Developer Mode enabled to copy the ID).

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
      channel_id: "5678..."         # Optional: send all messages to one channel (static ID)
      notify_channel_id: "9012..."   # Optional: send all messages to this channel ID (also DISCORD_NOTIFY_CHANNEL_ID env)
    discord_webhook:
      webhook_url: "https://discord.com/api/webhooks/..."
    slack:
      webhook_url: "https://hooks.slack.com/services/..."
```

| Channel | Required Settings | Description |
|---|---|---|
| `discord` | `token` + (`guild_id` or `channel_id`) | Discord Bot with rich embeds; auto-creates per-repo channels. Set `notify_channel_id` to route all notifications to a single dedicated channel instead. |
| `discord_webhook` | `webhook_url` | Posts to a Discord channel via webhook (simplest setup) |
| `slack` | `webhook_url` | Posts to a Slack channel via incoming webhook |

You can enable multiple channels simultaneously — all enabled channels receive every notification. For example, you can use `discord` for per-repo channels and add `discord_webhook` as an extra notification point to a shared ops channel.

### Run-Completion Notifications

Set `default-notify` (and optionally `default-notify-on`) in `/etc/eventic/config.yaml`. Eventic fires this template once per workflow run, after the run reaches a terminal state, with `{{.State}}` bound to `success` or `failure`:

```yaml
# In /etc/eventic/config.yaml
default-notify: "Workflow {{.State}} for {{.Repo}} ({{.Event}}.{{.Action}})"
default-notify-on: [failure]   # only notify on failure; omit to always notify
```

### Per-Node Notifications

For finer control, add a workflow node whose command sends the message itself — there is no special-casing, it is just another node:

```json
{
  "node_key": "alert",
  "name": "Notify deploy",
  "type": "run_command",
  "command": "eventic-client notify \"Deploy $EVENTIC_REPO finished: $RESULT\"",
  "continue_on_error": true
}
```

> **Note:** Run-completion notifications are only sent when a workflow actually runs. If no workflow resolves for an event (it is recorded as `skipped`), no notification is dispatched — even if `default-notify` is configured. This prevents noise from events that have no automation attached.

### Notification Filtering

Use `notify_on` to control when notifications fire:

| Value | Behavior |
|---|---|
| *(omitted)* | Always notify (default) |
| `[failure]` | Only notify on run failure |
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
| `{{.Stdout}}` | Combined output of the run's last node |
| `{{.Sender}}` | GitHub user (or `comms` sender) who triggered the event |
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
