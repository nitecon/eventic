# Workflows

Eventic is a per-repo workflow engine. When an event arrives (a GitHub webhook,
provider webhook, or `comms` message), the client normalizes provider-specific
payloads into a stable internal event when a mapping matches, resolves **one**
workflow for the event, checks out the branch, and executes the workflow's
**DAG** of typed action nodes, passing output and context between them.

Workflows live in the client's SQLite database and are authored through the
dashboard / [Workflow API](../README.md#workflow-api). There is **no** in-repo
config file — `.eventic.yaml` and `.deploy/deploy.yml` are no longer read.

## Model

A workflow is bound to one `(scope, repo, event_type)` slot and contains:

- **Nodes** — each performs one typed action, such as running a command, sending
  an HTTP request, sending a project message, or triggering an event on a project.
- **Edges** — directed, optionally-conditional links between nodes.

### Node fields

| Field | Description |
|---|---|
| `node_key` | Stable identifier referenced by edges (unique within the workflow) |
| `name` | Human-readable label |
| `type` | `run_command`, `send_http_request`, `send_project_message`, or `trigger_project_event` (`command` is accepted as a legacy alias for `run_command`) |
| `command` | Shell command executed via `sh -c` for `run_command` nodes |
| `capture` | When set, the node's trimmed stdout is stored under this variable name and exported to downstream nodes as `NAME=value` |
| `continue_on_error` | When `true`, a failure here does not fail the overall run and downstream `success` edges still see the node as failed |
| `timeout_seconds` | Per-node timeout (0 = no explicit timeout) |
| `config.retry.max_attempts` | Total attempts for transient step failures (default `1`, max `10`) |
| `config.retry.backoff_seconds` | Wait between failed attempts before the next retry (default `0`, max `3600`) |
| `pos_x` / `pos_y` | Editor canvas coordinates (UI only) |
| `config` | Typed action JSON: HTTP request settings, project message/event settings, status/exit-code evaluation, response handling, and post actions |

### Typed actions

Each node first evaluates action success before response handling:

| Action | Required fields | Success check |
|---|---|---|
| `run_command` | `command` | `result.success_exit_codes` (default `0`) |
| `send_http_request` | `config.http_request.method`, `url`, optional headers/body | `result.success_statuses` (default `200-299`) |
| `send_project_message` | `config.project_message.repo`, `message` | local dispatch accepted |
| `trigger_project_event` | `config.project_event.repo`, `event_type` | local dispatch accepted |

Response handling is configured under `config.response`:

| Mode | Behavior |
|---|---|
| `noop` | Leaves the action output/body as the node output |
| `send_data` | Selects one value using dot notation (for example `body.token`) and returns or sends that value |
| `iterate_data` | Selects a JSON array using dot notation (for example `body.items`) and iterates each item |

`config.post_actions` can run follow-up typed actions after the primary action
succeeds. With `send_data`, a post action sees `${EVENTIC_RESPONSE_DATA}`. With
`iterate_data`, post actions run once per item and see `${EVENTIC_ITEM}` plus
`${EVENTIC_ITEM_INDEX}`. Post actions use the same typed action names and
status/exit matcher rules as primary nodes.

### Edge conditions

An edge fires only if its condition is satisfied. A node becomes **eligible**
once all its parents are terminal, and **runs** only if at least one incoming
edge condition is satisfied; otherwise it is **skipped**, and skips propagate
(a skipped node satisfies none of its outgoing edges).

| Condition | Target runs when… |
|---|---|
| `always` (or empty) | the source node finished, success or failure |
| `success` | the source node succeeded |
| `failure` | the source node failed |
| `NAME == value` | captured variable `NAME` equals `value` (case-insensitive) |
| `NAME != value` | captured variable `NAME` does not equal `value` |

Root nodes (no parents) always start. An **unrecognized condition fails closed**
(the edge is treated as unsatisfied) so a malformed gate never silently fires.

The graph is validated at author time and run time: missing node references and
**cycles** (detected via Kahn topological sort) are rejected.

## Node environment contract

Every node command receives the process environment plus:

| Variable | Description |
|---|---|
| `EVENTIC_REPOS` | Root directory of managed repositories |
| `EVENTIC_REPO` | `org/repo` |
| `EVENTIC_REF` | Checked-out git ref |
| `EVENTIC_EVENT` | Stable event when mapped, otherwise the external event type |
| `EVENTIC_STABLE_EVENT` | Stable internal event key, when a mapping matched |
| `EVENTIC_PROVIDER` | Inbound provider (`github`, `gitlab`, `prometheus`, `custom`, etc.) |
| `EVENTIC_EXTERNAL_EVENT` | Provider event name before normalization |
| `EVENTIC_EXTERNAL_ACTION` | Provider action before normalization |
| `EVENTIC_ACTION` | Event action (e.g. `opened`) |
| `EVENTIC_SENDER` | Who triggered the event |
| `EVENTIC_MESSAGE` | Free-form payload from a `comms` event (empty otherwise) |
| `EVENTIC_CONTEXT_DIR` | Per-run scratch directory shared by all nodes in the run |
| `EVENTIC_PR_NUMBER` | PR number (0 if not a PR event) |
| `EVENTIC_DELIVERY_ID` | Delivery ID for tracing |
| `EVENTIC_RESPONSE_DATA` | Selected response data while a post action is running |
| `EVENTIC_ITEM` | Current iterated item while an `iterate_data` post action is running |
| `EVENTIC_ITEM_INDEX` | Current zero-based item index while an `iterate_data` post action is running |
| `<captured vars>` | Each upstream `capture` exported as `NAME=value` |

Use `EVENTIC_CONTEXT_DIR` to hand large artifacts (fetched documents, generated
reports) between nodes — write a file in one node, read it in the next. Use
`capture` for short, decision-grade values you want to gate edges on.

## Resolution

The client picks exactly one workflow, **repo overriding global**, most
specific first. If an inbound mapping produced a stable event such as
`artifact.initiate`, resolution uses that stable event with no action suffix.
If no mapping matched, legacy external event/action resolution is preserved:

1. `repo` scope + `event.action` (e.g. `pull_request.opened`)
2. `repo` scope + `event` (e.g. `pull_request`)
3. `global` scope + `event.action`
4. `global` scope + `event`

The first enabled match wins. If nothing matches, the event is recorded as
`skipped`. A repo defining its own workflow for an event opts **out** of the
global workflow for that event (no composition in v1).

Default stable mappings include GitHub/GitLab/Bitbucket main-branch pushes to
`artifact.initiate`, GitHub releases and GitLab/Bitbucket tag pushes to
`artifact.published`, failed GitHub workflow runs, GitLab pipelines,
Bitbucket commit statuses, and Prometheus firing alerts to `system.failure`,
GitHub Dependabot/code-scanning/security-advisory events, GitLab
vulnerabilities, and critical scan findings to `security.alarm`, and `comms`
or Discord adapter commands to `communication.received`. Edit mappings from
`/global` or the `/api/event-mappings` API. `/global` also exposes a visual
mapper: drag provider events from the left column onto stable events in the
right column to create mappings. Stable events live in a global registry and
can be created with custom keys such as `catchall`, `published`, or
`security.alarm`. Use provider `*` for mappings that should apply to any
inbound provider. Duplicate mappings with the same provider, stable event, and
condition set are rejected.

Every normalized inbound event is also written to the `inbound_events` audit
table. The `/global/events/history` page and `GET /api/events` show the
provider event, stable event, mapping status/id, severity metadata, final
state, and bounded raw payload. `POST /api/events/{id}/replay` replays the
recorded normalized event through the same dispatcher.

External systems that already know the intended internal event can call
`POST /event/{stable_event}` with Bearer-token auth and a JSON body containing
`repo`, optional `ref`, `title`, `body`, `action`, `sender`, and `metadata`.
That ingress sets `StableEvent` directly and bypasses provider mapping.

## Parallelism

Nodes that become eligible together run concurrently, bounded by
`max-node-workers` (client config, default `1`). All nodes in a run share **one
checkout**, so concurrent file-mutating nodes can race. Keep `max-node-workers`
at `1` unless your graph serializes working-tree mutations via edges, or your
parallel branches only read the tree / write to `EVENTIC_CONTEXT_DIR`.

## Example: agentic threat assessment

A `comms`-triggered workflow that researches a change, captures a verdict, and
conditionally files an issue:

```json
{
  "scope": "repo",
  "repo": "myorg/myrepo",
  "event_type": "comms",
  "name": "Threat assessment",
  "enabled": true,
  "nodes": [
    {
      "node_key": "rag",
      "name": "RAG fetch",
      "type": "run_command",
      "command": "rag-fetch --repo \"$EVENTIC_REPO\" --query \"$EVENTIC_MESSAGE\" > \"$EVENTIC_CONTEXT_DIR/context.md\""
    },
    {
      "node_key": "memory",
      "name": "Capture memory",
      "type": "run_command",
      "command": "memory context \"$EVENTIC_MESSAGE\" -k 10 >> \"$EVENTIC_CONTEXT_DIR/context.md\""
    },
    {
      "node_key": "assess",
      "name": "Gemini threat assess",
      "type": "run_command",
      "capture": "VERDICT",
      "command": "gemini -p \"Using $EVENTIC_CONTEXT_DIR/context.md, classify supply-chain risk. Reply with exactly one word: benign or risky.\""
    },
    {
      "node_key": "ticket",
      "name": "Open issue",
      "type": "run_command",
      "command": "gh issue create -R \"$EVENTIC_REPO\" -t \"Threat review: $EVENTIC_MESSAGE\" -F \"$EVENTIC_CONTEXT_DIR/context.md\""
    }
  ],
  "edges": [
    {"from_node": "rag",    "to_node": "memory", "condition": "success"},
    {"from_node": "memory", "to_node": "assess", "condition": "success"},
    {"from_node": "assess", "to_node": "ticket", "condition": "VERDICT == risky"}
  ]
}
```

Flow: `rag` writes context → `memory` appends to it → `assess` captures
`VERDICT` from the model's one-word reply → `ticket` runs **only** when
`VERDICT == risky`. A `benign` verdict leaves the `ticket` node skipped.

Trigger it with a `comms` message (see the
[`comms` event](../README.md#the-comms-event) docs):

```bash
curl -X POST https://your-server:8080/event/comms \
  -H "Authorization: Bearer your-comms-token" \
  -H "Content-Type: application/json" \
  -d '{"repo":"myorg/myrepo","ref":"main","message":"Assess the latest dependency bump","sender":"security-agent"}'
```
