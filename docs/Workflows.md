# Workflows

Eventic is a per-repo workflow engine. When an event arrives (a GitHub webhook
or a `comms` message), the client resolves **one** workflow for the event,
checks out the branch, and executes the workflow's **DAG** of CLI-command nodes,
passing output and context between them.

Workflows live in the client's SQLite database and are authored through the
dashboard / [Workflow API](../README.md#workflow-api). There is **no** in-repo
config file — `.eventic.yaml` and `.deploy/deploy.yml` are no longer read.

## Model

A workflow is bound to one `(scope, repo, event_type)` slot and contains:

- **Nodes** — each runs a single command via `sh -c`.
- **Edges** — directed, optionally-conditional links between nodes.

### Node fields

| Field | Description |
|---|---|
| `node_key` | Stable identifier referenced by edges (unique within the workflow) |
| `name` | Human-readable label |
| `type` | `command` (the only type in v1) |
| `command` | Shell command executed via `sh -c` |
| `capture` | When set, the node's trimmed stdout is stored under this variable name and exported to downstream nodes as `NAME=value` |
| `continue_on_error` | When `true`, a failure here does not fail the overall run and downstream `success` edges still see the node as failed |
| `timeout_seconds` | Per-node timeout (0 = no explicit timeout) |
| `pos_x` / `pos_y` | Editor canvas coordinates (UI only) |
| `config` | Free-form JSON for future node types |

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
| `EVENTIC_EVENT` | Event type (`push`, `pull_request`, `comms`, …) |
| `EVENTIC_ACTION` | Event action (e.g. `opened`) |
| `EVENTIC_SENDER` | Who triggered the event |
| `EVENTIC_MESSAGE` | Free-form payload from a `comms` event (empty otherwise) |
| `EVENTIC_CONTEXT_DIR` | Per-run scratch directory shared by all nodes in the run |
| `EVENTIC_PR_NUMBER` | PR number (0 if not a PR event) |
| `EVENTIC_DELIVERY_ID` | Delivery ID for tracing |
| `<captured vars>` | Each upstream `capture` exported as `NAME=value` |

Use `EVENTIC_CONTEXT_DIR` to hand large artifacts (fetched documents, generated
reports) between nodes — write a file in one node, read it in the next. Use
`capture` for short, decision-grade values you want to gate edges on.

## Resolution

The client picks exactly one workflow, **repo overriding global**, most
specific first:

1. `repo` scope + `event.action` (e.g. `pull_request.opened`)
2. `repo` scope + `event` (e.g. `pull_request`)
3. `global` scope + `event.action`
4. `global` scope + `event`

The first enabled match wins. If nothing matches, the event is recorded as
`skipped`. A repo defining its own workflow for an event opts **out** of the
global workflow for that event (no composition in v1).

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
      "type": "command",
      "command": "rag-fetch --repo \"$EVENTIC_REPO\" --query \"$EVENTIC_MESSAGE\" > \"$EVENTIC_CONTEXT_DIR/context.md\""
    },
    {
      "node_key": "memory",
      "name": "Capture memory",
      "type": "command",
      "command": "memory context \"$EVENTIC_MESSAGE\" -k 10 >> \"$EVENTIC_CONTEXT_DIR/context.md\""
    },
    {
      "node_key": "assess",
      "name": "Gemini threat assess",
      "type": "command",
      "capture": "VERDICT",
      "command": "gemini -p \"Using $EVENTIC_CONTEXT_DIR/context.md, classify supply-chain risk. Reply with exactly one word: benign or risky.\""
    },
    {
      "node_key": "ticket",
      "name": "Open issue",
      "type": "command",
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
