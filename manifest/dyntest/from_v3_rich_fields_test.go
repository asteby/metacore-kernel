package dyntest

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// journalManifestJSON mirrors accounting_lite's "create_journal_entry": a
// searchable picker (dynamic_select + ref) and a line-items array whose columns
// carry total flags and a balance rule. It is the case that regressed to plain
// inputs because the rich field properties were dropped in the v3 → host
// conversion.
const journalManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "accounting_lite", "name": "Accounting", "version": "0.1.15" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      {
        "key": "create_journal_entry",
        "label": "Crear asiento",
        "target_model": "JournalEntry",
        "handler": { "type": "compiled", "function": "OnJournalEntryCreate" },
        "placement": "create",
        "fields": [
          { "key": "journal_id", "label": "Diario", "type": "dynamic_select", "required": true, "ref": "Journal", "placeholder": "Buscar diario…" },
          {
            "key": "journal_entry_lines",
            "label": "Renglones",
            "type": "array",
            "required": true,
            "balance": { "debit_column": "debit", "credit_column": "credit", "message": "Σdébitos = Σcréditos" },
            "item_fields": [
              { "key": "account_id", "label": "Cuenta", "type": "dynamic_select", "required": true, "ref": "Account" },
              { "key": "debit", "label": "Débito", "type": "number", "total": true },
              { "key": "credit", "label": "Crédito", "type": "number", "total": true }
            ]
          }
        ]
      }
    ]
  }
}`

func TestFromV3_RichActionFieldsSurvive(t *testing.T) {
	m, err := v3.Parse([]byte(journalManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	actions := out.Actions["JournalEntry"]
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	fields := actions[0].Fields
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}

	// 1. Field key is populated (NOT empty) — keyed off v3 "key".
	diario := fields[0]
	if diario.Key != "journal_id" {
		t.Errorf("diario.Key = %q, want journal_id (empty key was the prod bug)", diario.Key)
	}
	if diario.Type != "dynamic_select" || diario.Ref != "Journal" || diario.Placeholder != "Buscar diario…" {
		t.Errorf("diario rich props lost: %+v", diario)
	}

	// 2. Line-items: item_fields + total + balance survive.
	lines := fields[1]
	if lines.Key != "journal_entry_lines" || lines.Type != "array" {
		t.Errorf("lines field = %+v", lines)
	}
	if len(lines.ItemFields) != 3 {
		t.Fatalf("item_fields len = %d, want 3 (line-items collapsed to plain input was the bug)", len(lines.ItemFields))
	}
	acct := lines.ItemFields[0]
	if acct.Key != "account_id" || acct.Type != "dynamic_select" || acct.Ref != "Account" {
		t.Errorf("nested account picker lost: %+v", acct)
	}
	if !lines.ItemFields[1].Total || !lines.ItemFields[2].Total {
		t.Errorf("total flags lost on debit/credit columns")
	}
	if lines.Balance == nil || lines.Balance.DebitColumn != "debit" || lines.Balance.CreditColumn != "credit" {
		t.Errorf("balance rule lost: %+v", lines.Balance)
	}

	// 3. Round-trip through the host action type (modelbase.ActionDef) — the
	// exact JSON path ops uses (KernelManifestToRecord) — preserves everything.
	raw, err := json.Marshal(out.Actions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var host map[string][]modelbase.ActionDef
	if err := json.Unmarshal(raw, &host); err != nil {
		t.Fatalf("unmarshal into modelbase: %v", err)
	}
	hf := host["JournalEntry"][0].Fields
	if hf[0].Key != "journal_id" || hf[0].Ref != "Journal" {
		t.Errorf("host diario lost key/ref: %+v", hf[0])
	}
	if len(hf[1].ItemFields) != 3 {
		t.Fatalf("host item_fields len = %d, want 3", len(hf[1].ItemFields))
	}
	if !hf[1].ItemFields[1].Total {
		t.Error("host lost total flag")
	}
	if hf[1].Balance == nil || hf[1].Balance.CreditColumn != "credit" {
		t.Errorf("host lost balance: %+v", hf[1].Balance)
	}
}

// lockRowsManifestJSON mirrors a "receive goods" action: a line-items array
// prefilled from the ordered lines whose rows are FIXED (lock_rows:true) — the
// user may only edit each row's received quantity, not add or delete rows.
const lockRowsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "inventory", "name": "Inventory", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      {
        "key": "receive_goods",
        "label": "Recibir mercancía",
        "target_model": "Receipt",
        "handler": { "type": "compiled", "function": "OnReceiveGoods" },
        "placement": "row",
        "fields": [
          {
            "key": "lines",
            "label": "Renglones",
            "type": "array",
            "lock_rows": true,
            "item_fields": [
              { "key": "product_id", "label": "Producto", "type": "dynamic_select", "ref": "Product" },
              { "key": "received_qty", "label": "Recibido", "type": "number" }
            ]
          }
        ]
      }
    ]
  }
}`

// TestFromV3_LockRowsSurvives verifies a type:"array" field can declare
// lock_rows:true under the STRICT v3 schema and that the flag survives the
// v3 → host conversion and the host JSON round-trip served to the frontend.
func TestFromV3_LockRowsSurvives(t *testing.T) {
	m, err := v3.Parse([]byte(lockRowsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse (strict schema must accept lock_rows): %v", err)
	}

	// 1. v3 struct carries the flag.
	af := m.Contributions.Actions[0].Fields[0]
	if af.Key != "lines" || af.Type != "array" {
		t.Fatalf("unexpected field: %+v", af)
	}
	if !af.LockRows {
		t.Errorf("v3 ActionField.LockRows lost: %+v", af)
	}

	// 2. v3 → host conversion preserves it.
	out := manifest.FromV3(m)
	lines := out.Actions["Receipt"][0].Fields[0]
	if !lines.LockRows {
		t.Errorf("host FieldDef.LockRows lost after FromV3: %+v", lines)
	}

	// 3. Host JSON round-trip (the exact path ops serves to the SDK) keeps it.
	raw, err := json.Marshal(out.Actions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var host map[string][]modelbase.ActionDef
	if err := json.Unmarshal(raw, &host); err != nil {
		t.Fatalf("unmarshal into modelbase: %v", err)
	}
	if !host["Receipt"][0].Fields[0].LockRows {
		t.Errorf("lock_rows dropped in host round-trip served to the frontend")
	}
}
