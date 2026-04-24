// Package flow is the Metacore workflow engine: a generic DAG executor with a
// pluggable node registry, template interpolation, and optional persistence.
//
// Hosts (ops, link, future web apps) import flow to stop re-implementing the
// same engine/registry/context patterns in every app. The kernel ships with a
// set of domain-free built-in nodes (HTTP, Webhook, Condition, Switch, Delay,
// Loop, Filter, Set-Variable, Transform, Split, Merge, Error-Handler, Note,
// Trigger); apps layer domain nodes on top via Engine.RegisterNode.
//
// # Quick start
//
//	// 1. Build an engine. Config is all-optional.
//	engine := flow.NewEngine(flow.Config{
//	    Store:    myGormStore{db: db},   // optional persistence
//	    Progress: myWSPublisher{hub: h}, // optional WS notifications
//	    Logger:   log.Default(),         // optional logger
//	})
//
//	// 2. Register the app's own node executors.
//	engine.RegisterNode("message", myapp.MessageNode{svc: messaging})
//	engine.RegisterNode("create_ticket", myapp.CreateTicketNode{db: db})
//
//	// 3. Run a flow.
//	flowDef := mapDBRecordToKernelFlow(dbRecord)
//	exec, err := engine.ExecuteFlow(flowDef, flow.TriggerManual,
//	    map[string]interface{}{"foo": "bar"}, nil)
//
// # Node contract
//
// A NodeExecutor returns a NodeResult whose fields drive traversal:
//
//   - Output         → variables to merge into the ExecutionContext
//   - OutputHandle   → single edge to follow (branching: Condition / Switch)
//   - OutputHandles  → multiple edges to follow (Split)
//   - Stop           → halt execution cleanly
//
// # Template interpolation
//
// Any string in a node config can reference context variables using the
// `{{var}}` syntax. Dot-notation is supported for one level
// (`{{contact.name}}`), and a small set of suffix functions is available
// (`{{name.uppercase()}}`, `{{text.trim()}}`).
//
// # Trigger service
//
// When apps want declarative "run flow X when event Y occurs" semantics,
// TriggerService coordinates:
//
//   - FlowLoader returns the candidate flows for (org, triggerType)
//   - TriggerMatcher per trigger type decides whether a given flow matches
//     the incoming event (keyword, welcome, etc.)
//   - Dispatch / DispatchAll invoke the engine for matches
//
// The kernel ships with pass-through matchers for manual / webhook / api /
// schedule / event. Apps register custom matchers (link provides keyword,
// welcome, menu, fallback) on top.
//
// # What the kernel does NOT do
//
//   - It does not persist flows. Apps own their storage (GORM, YAML, API).
//   - It does not expose HTTP handlers. Apps decide how to trigger executions.
//   - It does not know about contacts, tickets, messaging, AI. Those live in
//     app-provided NodeExecutors.
package flow
