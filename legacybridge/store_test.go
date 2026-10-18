package legacybridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestStores_Conformance(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// Refs: dos índices únicos.
		r := Ref{Org: org, System: "erp", Entity: "customers", ExternalID: "erp:clients:1", LocalID: "L1", SourceVersion: "v1"}
		if err := s.PutRef(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.SourceVersion = "v2"
		if err := s.PutRef(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, ok, _ := s.GetRefByLocal(ctx, org, "erp", "customers", "L1")
		if !ok || got.SourceVersion != "v2" || got.Owner != OwnerLegacy || got.State != RefSynced {
			t.Fatalf("ref = %+v", got)
		}
		if err := s.PutRef(ctx, Ref{Org: org, System: "erp", Entity: "customers", ExternalID: "erp:clients:2", LocalID: "L1"}); !errors.Is(err, ErrRefConflict) {
			t.Fatalf("local_id duplicado: %v", err)
		}
		if err := s.PutRef(ctx, Ref{Org: org, System: "erp", Entity: "customers", LocalID: "L9"}); err == nil {
			t.Fatal("ref sin external_id")
		}
		if _, ok, _ := s.GetRef(ctx, "otra-org", "erp", "customers", "erp:clients:1"); ok {
			t.Fatal("los refs son por organización")
		}

		// Cursor.
		if c, _ := s.GetCursor(ctx, org, "erp", "default"); c != "" {
			t.Fatal("cursor inicial vacío")
		}
		_ = s.SetCursor(ctx, org, "erp", "default", "c1")
		_ = s.SetCursor(ctx, org, "erp", "default", "c2")
		if c, _ := s.GetCursor(ctx, org, "erp", "default"); c != "c2" {
			t.Fatalf("cursor = %q", c)
		}

		// Propiedad y fase.
		_ = s.PutRule(ctx, OwnershipRule{Org: org, System: "erp", Branch: "b5", Owner: OwnerLegacy})
		_ = s.PutRule(ctx, OwnershipRule{Org: org, System: "erp", Branch: "b5", Owner: OwnerLocal, Note: "flip b5", UpdatedBy: "u"})
		rules, _ := s.Rules(ctx, org, "erp")
		if len(rules) != 1 || rules[0].Owner != OwnerLocal || rules[0].Entity != Wildcard {
			t.Fatalf("reglas = %+v", rules)
		}
		if err := s.PutRule(ctx, OwnershipRule{Org: org, System: "erp", Owner: "x"}); err == nil {
			t.Fatal("dueño inválido")
		}
		_ = s.SetPhase(ctx, org, "erp", "F1", "u")
		if p, _ := s.Phase(ctx, org, "erp"); p != "F1" {
			t.Fatalf("fase = %q", p)
		}
		if g, ok := s.(*GormStore); ok {
			var n int64
			g.db.Raw(`SELECT COUNT(*) FROM ` + TableOwnershipLog + ` WHERE new_owner = 'local' AND old_owner = 'legacy'`).Scan(&n)
			if n != 1 {
				t.Fatalf("bitácora del flip = %d", n)
			}
		}

		// En espera.
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		env := Envelope{Source: "erp", Entity: "e", ExternalID: "x1", Op: OpUpsert, SourceVersion: "v", Aggregate: Aggregate{Root: map[string]any{"a": "b"}}}
		_ = s.PutPending(ctx, PendingEnvelope{Org: org, System: "erp", Envelope: env, Missing: []Dependency{{Entity: "p", ExternalID: "1"}}, Attempts: 1, FirstSeen: now, NextAttempt: now.Add(time.Minute)})
		if due, _ := s.DuePending(ctx, now, 10); len(due) != 0 {
			t.Fatal("no vence todavía")
		}
		due, _ := s.DuePending(ctx, now.Add(2*time.Minute), 10)
		if len(due) != 1 || due[0].Envelope.Aggregate.Root["a"] != "b" || len(due[0].Missing) != 1 {
			t.Fatalf("vencidos = %+v", due)
		}
		_ = s.DeletePending(ctx, org, "erp", "e", "x1")
		if n, _ := s.CountPending(ctx, org, "erp"); n != 0 {
			t.Fatal("borrado")
		}

		// Comandos: cabeza por agregado y agregados vencidos.
		mk := func(agg, key string) Command {
			c, created, err := s.InsertCommand(ctx, Command{Org: org, System: "erp", Kind: "k", AggregateKey: agg, IdempotencyKey: key, Payload: json.RawMessage(`{}`)})
			if err != nil || !created {
				t.Fatalf("insert %s: %v", key, err)
			}
			return c
		}
		a1, a2, b1 := mk("A", "a1"), mk("A", "a2"), mk("B", "b1")
		if a2.Seq != a1.Seq+1 || b1.Seq != 1 {
			t.Fatalf("seq = %d %d %d", a1.Seq, a2.Seq, b1.Seq)
		}
		if _, created, _ := s.InsertCommand(ctx, Command{Org: org, System: "erp", Kind: "k", AggregateKey: "A", IdempotencyKey: "a1", Payload: json.RawMessage(`{}`)}); created {
			t.Fatal("idempotencia")
		}
		far := time.Now().Add(time.Hour)
		dueAggs, _ := s.DueAggregates(ctx, far, 10)
		if len(dueAggs) != 2 {
			t.Fatalf("agregados vencidos = %+v", dueAggs)
		}
		a1.Status = CommandDead
		_ = s.UpdateCommand(ctx, a1)
		if head, _, _ := s.HeadCommand(ctx, AggregateRef{Org: org, System: "erp", AggregateKey: "A"}); head.ID != a1.ID {
			t.Fatal("un dead sigue siendo la cabeza (bloquea)")
		}
		dueAggs, _ = s.DueAggregates(ctx, far, 10)
		if len(dueAggs) != 1 || dueAggs[0].AggregateKey != "B" {
			t.Fatalf("A bloqueado no vence: %+v", dueAggs)
		}
		a1.Status = CommandSent
		_ = s.UpdateCommand(ctx, a1)
		if head, _, _ := s.HeadCommand(ctx, AggregateRef{Org: org, System: "erp", AggregateKey: "A"}); head.ID != a2.ID {
			t.Fatal("tras entregar a1 la cabeza es a2")
		}
		if n, _ := s.CountCommands(ctx, org, "erp", CommandPending); n != 2 {
			t.Fatalf("pendientes = %d", n)
		}
		if err := s.UpdateCommand(ctx, Command{ID: "nope"}); !errors.Is(err, ErrCommandNotFound) {
			t.Fatalf("update inexistente: %v", err)
		}
	})
}
