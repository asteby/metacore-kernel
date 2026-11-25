# `handoff_to_human`: puerto opcional `HandoffSink`

La herramienta `handoff_to_human` (`agent/core_tools.go`) transfiere la
conversacion a una persona. Por defecto no cambia: devuelve
`status=handoff_initiated` y `action=DISABLE_BOT`. Un host puede inyectar un
**`HandoffSink`** para decidir a donde va el traspaso (una mesa de soporte, una
cola) sin crear otra herramienta: no existe `escalate_to_support`.

```go
type HandoffSink interface {
    Handoff(ctx context.Context, req HandoffRequest) (HandoffResult, error)
}
// HandoffRequest{ConversationID, Reason, Severity, Transcript []contract.Turn}
// HandoffResult{Reference, Status}
```

`Transcript` reutiliza `support/contract.Turn`; `Severity` se normaliza con
`contract.NormalizeSeverity` (un valor desconocido o ausente es `normal`).

## Uso

```go
tool := agent.NewHandoffToHumanTool(
    agent.WithHandoffSink(mySink),
    agent.WithHandoffDescription("..."),            // quien es el humano, por config
    agent.WithHandoffRedactor(redactFn),            // politica de redaccion del host
    agent.WithHandoffTranscriptLimits(40, 2000),    // ultimos N turnos, M runas c/u
)
```

Sin opciones (o con `WithHandoffSink(nil)`) el resultado, los parametros y la
descripcion son byte a byte los de siempre. Con sink, el parametro opcional
`severity` aparece en `Parameters()`.

## Garantias

- El sink se llama **una vez** por ejecucion, con la razon y el transcript del
  historial de la conversacion: sin turnos vacios, pasado por el redactor del
  host (si hay), cada turno cortado con `TruncateRunes` y solo los ultimos
  `maxTurns` (defecto 40 turnos de 2000 runas). El kernel no trae redactor
  propio: la politica de redaccion es del host.
- Exito: el resultado de siempre mas `reference` y `case_status` si el sink los
  devolvio.
- Error o panico del sink: resultado controlado `status=handoff_failed` con un
  `error` generico (sin el detalle interno), **sin** `action` ni `message`: el
  bot no se apaga y la conversacion sigue; el modelo ve el fallo.
- Contexto cancelado: la herramienta devuelve `ctx.Err()` y no llama al sink.
