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

---

## Client

The client is a small, self-contained binary that runs as a systemd service on any Linux machine. It maintains a persistent WebSocket connection to the server, automatically reconnects with exponential backoff, and executes hooks when events arrive.

### Installation

Download the latest release for your platform from the [Releases page](https://github.com/nitecon/eventic/releases):

```bash
# Download the binary
curl -L -o /usr/local/bin/eventic-client \
  https://github.com/nitecon/eventic/releases/latest/download/eventic-client-linux-amd64

chmod +x /usr/local/bin/eventic-client
```

### Configuration

Create `/etc/eventic/config.yaml`:

```yaml
relay: "wss://your-server:8080/ws"
token: "your-auth-token"
client_id: "build-server-01"
repos_dir: "/opt/eventic/repos"
subscribe:
  - "myuser/*"
  - "myworkorg/*"
  - "kubernetes/specific-repo"
auto-update: true
global-hooks:
  pre: "echo preparing ${EVENTIC_REPO}..."
  post: "echo done with ${EVENTIC_REPO}"
```

| Field | Description |
|---|---|
| `relay` | WebSocket URL of the Eventic server |
| `token` | Authentication token (must be listed in the server's `EVENTIC_CLIENT_TOKENS`) |
| `client_id` | Unique identifier for this client |
| `repos_dir` | Directory where repos will be cloned and managed |
| `subscribe` | List of glob patterns to filter which repositories trigger hooks (see [Subscription Patterns](#subscription-patterns)) |
| `auto-update` | When `true`, the client checks for new releases every 5 minutes and updates itself automatically |
| `global-hooks.pre` | Fallback pre hook — runs for repos that have no `.eventic.yaml` or `.deploy/deploy.yml` |
| `global-hooks.post` | Fallback post hook — runs for repos that have no `.eventic.yaml` or `.deploy/deploy.yml` |

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

### Systemd Service

Create `/etc/systemd/system/eventic-client.service`:

```ini
[Unit]
Description=Eventic Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/eventic-client /etc/eventic/config.yaml
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
sudo mkdir -p /opt/eventic/repos /etc/eventic
sudo systemctl daemon-reload
sudo systemctl enable --now eventic-client
```

### Auto-Update

When `auto-update: true` is set in the client config, the client will check GitHub releases every 5 minutes for a newer version. If one is found, it downloads the matching binary for the current OS and architecture, replaces itself in-place, and exits. The systemd service (configured with `Restart=always`) then restarts automatically with the new binary.

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
