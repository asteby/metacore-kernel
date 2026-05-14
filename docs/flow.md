# Flow Engine — `runtime/flow`

`runtime/flow` is the Metacore workflow engine: a generic DAG executor with a
pluggable node registry, template interpolation, and optional persistence.

It exists so apps stop re-implementing the same engine/registry/context patterns
in every host. The kernel ships with a set of domain-free built-in nodes
(HTTP, Webhook, Condition, Switch, Delay, Loop, Filter, SetVariable, Transform,
Split, Merge, ErrorHandler, Note, Trigger). Apps layer their own domain nodes
(`message`, `ai_chat`, `create_ticket`, …) on top via `Engine.RegisterNode`.

The package is **transport-agnostic** and **persistence-agnostic**:

- It does **not** persist flows. Apps own their storage (GORM, YAML, API).
- It does **not** know about contacts, tickets, messaging, AI — those live in
  app-provided `NodeExecutor`s.
- It exposes an **optional** Fiber `Handler` for the runtime endpoints
  (`/run`, `/test`, `/cancel`) but flow CRUD is left to the host.

---

## Quick start

```go
import "github.com/asteby/metacore-kernel/runtime/flow"

// 1. Build an engine. Config is all-optional.
engine := flow.NewEngine(flow.Config{
    Store:    myGormStore{db: db},   // optional persistence
    Progress: myWSPublisher{hub: h}, // optional WS notifications
    Logger:   log.Default(),         // optional logger
})

// 2. Register the app's own node executors.
engine.RegisterNode("message", myapp.MessageNode{svc: messaging})
engine.RegisterNode("create_ticket", myapp.CreateTicketNode{db: db})

// 3. Run a flow.
flowDef := mapDBRecordToKernelFlow(dbRecord)
exec, err := engine.ExecuteFlow(flowDef, flow.TriggerManual,
    map[string]interface{}{"foo": "bar"}, nil)
```

---

## DSL spec

A `Flow` is a plain Go struct. Apps map their persistence model (GORM record,
YAML, etc.) into a `*flow.Flow` before calling `ExecuteFlow`.

### Flow

| Field            | Type                                | Description                                                  |
| ---------------- | ----------------------------------- | ------------------------------------------------------------ |
| `ID`             | `uuid.UUID`                         | Flow identifier.                                             |
| `OrganizationID` | `uuid.UUID`                         | Owning organization (carried through `ExecutionContext`).    |
| `Name`           | `string`                            | Human label.                                                 |
| `Status`         | `FlowStatus`                        | `draft` / `active` / `paused` / `archived`.                  |
| `TriggerType`    | `TriggerType`                       | How the flow is entered (manual, webhook, schedule, event…). |
| `TriggerConfig`  | `map[string]interface{}`            | Trigger-type specific config.                                |
| `Nodes`          | `[]FlowNode`                        | Vertices of the DAG.                                         |
| `Edges`          | `[]FlowEdge`                        | Directed connections.                                        |
| `Variables`      | `[]FlowVariable`                    | Flow-level defaults seeded into the context.                 |
| `Settings`       | `map[string]interface{}`            | Free-form host settings.                                     |

### FlowNode

| Field  | Type           | Description                                                          |
| ------ | -------------- | -------------------------------------------------------------------- |
| `ID`   | `string`       | Unique per flow (e.g. `"set-greeting-1"`).                           |
| `Type` | `NodeType`     | Looked up in the engine's `Registry`.                                |
| `Data` | `FlowNodeData` | Configuration (Label, Config, TriggerConfig, retry/timeout settings) |

### FlowEdge

| Field          | Type             | Description                                                    |
| -------------- | ---------------- | -------------------------------------------------------------- |
| `Source`       | `string`         | Source node ID.                                                |
| `Target`       | `string`         | Target node ID.                                                |
| `SourceHandle` | `string`         | Output handle (e.g. `"true"`/`"false"` for Condition).         |
| `Condition`    | `*EdgeCondition` | Optional runtime check evaluated before traversing this edge.  |

### Built-in NodeTypes

| NodeType         | Purpose                                                                    |
| ---------------- | -------------------------------------------------------------------------- |
| `trigger`        | DAG entry point (engine always runs this first).                           |
| `http_request`   | Outbound HTTP call with template-expanded URL, headers, body.              |
| `webhook`        | Alias for `http_request` defaulting to `POST`.                             |
| `condition`      | Branch on `(field, operator, value)`; outputs `"true"`/`"false"`.          |
| `switch`         | Multi-case branch; outputs `case-i` / `default`.                           |
| `delay`/`wait`   | Sleep for N seconds (capped at 300).                                       |
| `loop`           | Iterate over `$source` array; seeds `$loop.count`, `$loop.items`.          |
| `filter`         | Filter array items by `(field, operator, value)`.                          |
| `set_variable`   | Write a named variable into the context.                                   |
| `transform_data` | Apply uppercase / lowercase / trim transforms.                             |
| `split`          | Follow multiple outgoing edges in parallel.                                |
| `merge`          | Visual join marker (no-op at runtime).                                     |
| `error_handler`  | Action `stop` / `retry` / `continue`.                                      |
| `note`           | Visual annotation (no-op at runtime).                                      |

App-specific nodes (e.g. `message`, `ai_chat`, `create_ticket`) are not part of
the kernel — register them on the engine with `Engine.RegisterNode`.

### Trigger types

The engine treats `TriggerType` as an opaque string; the kernel only defines
a generic baseline (`manual`, `webhook`, `schedule`, `event`, `api`). Hosts
may declare app-specific trigger types (`keyword`, `welcome`, `menu`,
`fallback`, …) and plug them into a `TriggerService` via custom
`TriggerMatcher` implementations.

### Templates

Any string in a node config can reference context variables using `{{var}}`
syntax. Dot notation is supported (`{{contact.name}}`) and a small set of
suffix functions: `{{name.uppercase()}}`, `{{text.trim()}}`,
`{{value.lowercase()}}`, `{{value.length()}}`.

System variables seeded automatically: `$now`, `$today`, `$trigger.type`,
`$trigger.data`, `$execution.id`, `$workflow.id`, `$workflow.name`.

---

## Node contract

A `NodeExecutor` returns a `*NodeResult` whose fields drive traversal:

```go
type NodeResult struct {
    Output        map[string]interface{} // variables merged into context
    OutputHandle  string                 // single edge to follow (branching)
    OutputHandles []string               // multiple edges to follow (split)
    Stop          bool                   // halt execution cleanly
}
```

Output values are stored under `"{nodeID}.{key}"` and — when the node declares
`Data.OutputVariables` — also under the declared variable name at top-level.

---

## Trigger service

When apps want declarative *"run flow X when event Y occurs"* semantics,
`TriggerService` coordinates:

- `FlowLoader` returns the candidate flows for `(org, triggerType)`.
- `TriggerMatcher` per trigger type decides whether a given flow matches
  the incoming event (keyword, welcome, etc.).
- `Dispatch` / `DispatchAll` invoke the engine for matches.

```go
svc := flow.NewTriggerService(engine, myLoader)
svc.RegisterMatcher("keyword", myapp.KeywordMatcher{})

exec, err := svc.Dispatch(ctx, flow.TriggerEvent{
    OrganizationID: orgID,
    Type:           "keyword",
    Payload:        map[string]interface{}{"message": text},
    AppContext:     map[string]interface{}{"contact_id": contactID},
})
```

---

## HTTP endpoints (optional)

The kernel ships an opt-in `Handler` so apps can expose the runtime over Fiber
without re-writing boilerplate:

```go
h := flow.NewHandler(flow.HandlerConfig{
    Engine:      engine,
    FlowSource:  myStore,            // implements GetFlow(orgID, flowID)
    OrgResolver: auth.OrgID,         // extracts orgID from request
})
h.Mount(api.Group("/flows"))
```

Routes:

| Method | Path           | Description                                                |
| ------ | -------------- | ---------------------------------------------------------- |
| POST   | `/:id/run`     | Asynchronous execution — returns the execution ID.         |
| POST   | `/:id/test`    | Synchronous "Test" run — returns inline `TestResult`.      |
| POST   | `/:id/cancel`  | Cancel a running execution (`:id` is the execution ID).    |

Flow CRUD (Create / Update / Delete) is left to the host because flows live in
the host's database with host-specific columns (versioning, statistics, etc.).

Body schema for `/run` and `/test`:

```json
{
  "trigger_data": { "...": "..." },
  "app_context":  { "...": "..." }
}
```

Response envelope follows the kernel convention:

```json
{ "success": true,  "data": { "...": "..." } }
{ "success": false, "message": "..." }
```

---

## What the kernel does NOT do

- Persist flows. Apps own their storage.
- Expose flow CRUD over HTTP. Apps decide the persistence schema.
- Know about contacts, tickets, messaging, AI. Those live in
  app-provided `NodeExecutor`s.
- Schedule flows (cron, queues). Apps wire a scheduler that calls
  `engine.ExecuteFlow` on tick.
