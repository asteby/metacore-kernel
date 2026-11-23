# Inline relations: a parent and its child rows in one call

A `one_to_many` relation declared with `create_inline` lets the create flow
capture the child rows together with the parent record. The client posts them
under the reserved key `_children`; the kernel's `dynamic.Service.Create` writes
the parent and then every child, all-or-nothing. It is the general form of the
document lines of [document-forms.md](document-forms.md) (a record plus its
rows) for any composition relation, and it reuses their machinery.

Without `_children` a create behaves exactly as before, and a manifest that
declares neither flag validates and serves the same payload as before.

## Manifest contract

On a `models[].relations[]` entry (v3 `ModelRelation`, legacy
`manifest.RelationDef`):

| Field | Type | Meaning |
| --- | --- | --- |
| `create_inline` | bool | The create form captures this relation's rows with the parent. Default `false`. |
| `min` | int >= 0 | Minimum child rows when `_children` is sent. Only with `create_inline`. |
| `addon` | string | Key of the addon that **owns** the child model (`through`) when it is not this manifest's own. Omitted = same manifest. See [Children of another addon](#children-of-another-addon). |
| `widget` | string | Key of a registered frontend relation widget that replaces the default inline sub-table (it receives the live form values). Pure UI metadata; omitted = the sub-table. |

Validation (`v3.Validate`, `Manifest.Validate`, and the strict JSON schema in
`manifest/v3/schema/manifest-v3.schema.json` / `docs/spec/v3/`):

- `create_inline` requires `kind: one_to_many`, `embed: true` and no `readonly`.
- `min` must be >= 0 and requires `create_inline`.
- `through` must be a model of the same manifest and `foreign_key` one of its
  columns (the runtime injects the parent id there) — unless `addon` is set.
- `addon` must be listed in `compatibility.requires` (optional or not): the
  child model belongs to it. With `addon` the model is not checked against this
  manifest (the kernel validates its columns when the child is written); a
  `rollup` over such a relation is rejected (it needs the child's columns).
- `widget` matches `^[a-z][a-z0-9_.-]*# Inline relations: a parent and its child rows in one call

A `one_to_many` relation declared with `create_inline` lets the create flow
capture the child rows together with the parent record. The client posts them
under the reserved key `_children`; the kernel's `dynamic.Service.Create` writes
the parent and then every child, all-or-nothing. It is the general form of the
document lines of [document-forms.md](document-forms.md) (a record plus its
rows) for any composition relation, and it reuses their machinery.

Without `_children` a create behaves exactly as before, and a manifest that
declares neither flag validates and serves the same payload as before.

## Manifest contract

On a `models[].relations[]` entry (v3 `ModelRelation`, legacy
`manifest.RelationDef`):

| Field | Type | Meaning |
| --- | --- | --- |
| `create_inline` | bool | The create form captures this relation's rows with the parent. Default `false`. |
| `min` | int >= 0 | Minimum child rows when `_children` is sent. Only with `create_inline`. |
| `addon` | string | Key of the addon that **owns** the child model (`through`) when it is not this manifest's own. Omitted = same manifest. See [Children of another addon](#children-of-another-addon). |
| `widget` | string | Key of a registered frontend relation widget that replaces the default inline sub-table (it receives the live form values). Pure UI metadata; omitted = the sub-table. |

Validation (`v3.Validate`, `Manifest.Validate`, and the strict JSON schema in
`manifest/v3/schema/manifest-v3.schema.json` / `docs/spec/v3/`):

- `create_inline` requires `kind: one_to_many`, `embed: true` and no `readonly`.
- `min` must be >= 0 and requires `create_inline`.
.

```json
{
  "key": "DiscountRule",
  "relations": [
    {
      "name": "tiers",
      "kind": "one_to_many",
      "through": "DiscountRuleTier",
      "foreign_key": "discount_rule_id",
      "embed": true,
      "create_inline": true,
      "min": 1
    }
  ]
}
```

`create_inline` and `min` are served on `TableMetadata.relations[]`
(`RelationMeta.create_inline`, `RelationMeta.min`) so the SDK renders the inline
sub-table in the create form and enforces the minimum on the client too.

## Host wiring

Like `DocumentLinesResolver`, the engine learns a model's inline relations from
the host:

```go
dynamic.Config{
    InlineRelationsResolver: func(ctx context.Context, model string) ([]dynamic.InlineRelation, bool) {
        def, ok := registry.ModelDefinition(model)
        if !ok {
            return nil, false
        }
        rels := dynamic.DeriveInlineRelations(def) // manifest.ModelDefinition
        return rels, len(rels) > 0
    },
}
```

Without the resolver no relation is inline: a request carrying `_children` is
rejected with the 422 below rather than silently dropping the rows. Hosts that
build `modelbase.RelationDef` from the manifest must copy `CreateInline` and
`Min` for the metadata projection.

## The `_children` payload

```json
POST /api/data/DiscountRule
{
  "name": "Mayoreo",
  "kind": "cart",
  "_children": {
    "tiers": [
      { "min_amount": 1000, "percent": 5 },
      { "min_amount": 5000, "percent": 10 }
    ]
  }
}
```

`_children` is an object keyed by relation name; each value is a list of row
objects (the child model's own columns). What the kernel does, in order:

1. Takes `_children` out of the input before anything else (it is not a column).
   The caller's original input, `_children` included, is what an approval
   replays, so a parent parked by a guard (`request_approval`) is created with
   its children when approved.
2. Validates the shape (errors below), merged with the parent's own field
   errors in one 422.
3. Checks the caller may create the child model of every relation that has rows;
   otherwise the parent is not written (403, like any create).
4. Creates the parent as usual.
5. Creates each child through the regular `Create` of the child model, so its
   formulas, rollups, hooks, events, validation and permissions all run. The
   runtime owns `id`, the relation's `foreign_key` (the parent id) and the
   values of the relation's `scope`: whatever the client sent for them is
   overwritten.
6. Re-reads the parent so the response carries the rollups the children
   changed.

Rows of a child are not allowed to carry their own `_children` (one level only).

`min` applies when `_children` is present: every `create_inline` relation of the
model must then carry at least `min` rows, even if the relation key is missing
from the object. A create without `_children` skips the check (API clients that
create the parent alone keep working).

## Errors

All are `422` with the usual `{ "errors": { "<key>": [ { "code", "params",
"message" } ] } }` body (`dynamic.ValidationError`):

| Key | Code | When |
| --- | --- | --- |
| `_children` | `invalid_type` | `_children` is not an object |
| `_children.<relation>` | `invalid_relation` | the relation does not exist or does not declare `create_inline` |
| `_children.<relation>` | `invalid_type` | the value is not a list |
| `_children.<relation>` | `min` | fewer rows than `min` (`params`: `min`, `count`) |
| `_children.<relation>.<i>` | `invalid_type` | row `i` is not an object |
| `_children.<relation>.<i>._children` | `invalid_type` | nested `_children` |
| `_children.<relation>.<i>.<field>` | the child's own | row `i` failed the child's validation / hook / guard |

`<i>` is the index in the posted list, so the SDK can mark the row.

## Atomicity

The parent and its children are not one SQL transaction (each goes through the
regular `Create`); the guarantee is compensation, the same as document lines:
when a child fails the children already written and the parent are removed
(hard delete) and the error comes back keyed by the failing row. The parent's
`created` event is only published after every child was written. Children
events already published before a later row failed are not retracted.

## Example

`customers.DiscountRule` declares the `tiers` relation above. A `cart` rule is
created in one call with its tiers; a rule without tiers is rejected with
`_children.tiers: min` instead of being saved as a rule that does nothing. A
tier that fails (`percent` missing) answers `_children.tiers.1.percent` and
nothing is left behind.

## Children of another addon

A product's per-market prices live in another addon's model
(`PriceListItem` of `customers`). Declare the relation with `addon`:

```json
{ "name": "market_prices", "kind": "one_to_many", "through": "PriceListItem",
  "foreign_key": "product_id", "embed": true, "create_inline": true,
  "addon": "customers", "widget": "market_prices" }
```

The runtime is the same as for any child: the row is written through
`Service.Create` of the child model, resolved by name, so it keeps its own
validation, hooks, permissions and organization scope, the foreign key and id
are stamped by the runtime, and everything is compensated if any row fails.
`addon` only changes (1) the manifest validation and (2) the error when the
model is not registered: the create is refused **before writing anything** with
the field error `_children.<relation>` / code `child_unavailable`
(params `model`, `addon`). The relation is served in
`/api/metadata/table/:model` as `relations[].addon` / `.widget`.
