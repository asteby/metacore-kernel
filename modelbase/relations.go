package modelbase

// RelationDef declares an inter-model edge rooted at the owning ModelDefiner.
// It mirrors `manifest.RelationDef` so addons (declarative manifests) and
// compiled core models share one vocabulary the metadata service can
// consume uniformly.
//
//	Kind = "one_to_many"  — owner has many rows on Through; ForeignKey is the
//	                        column on Through pointing back at the owner.
//	                        References defaults to "id". Pivot empty.
//	Kind = "many_to_many" — Pivot is the join table; ForeignKey is the column
//	                        on Pivot pointing at the owner; Through is the
//	                        target model.
//	Kind = "belongs_to"   — owner carries the FK column itself (ForeignKey),
//	                        pointing at Through.References (default "id").
//	                        Used by ColumnDef.Ref auto-derivation: a column
//	                        whose name matches a belongs_to FK reports
//	                        Ref=<Through> automatically.
//
// The metadata service uses these to:
//   - auto-derive ColumnDef.Ref on FK columns so the SDK can render a
//     reference-aware select without per-column wiring,
//   - expose relations to the SDK for <DynamicRelation> consumption.
type RelationDef struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Through    string `json:"through"`
	ForeignKey string `json:"foreign_key"`
	References string `json:"references,omitempty"`
	Pivot      string `json:"pivot,omitempty"`

	// Scope is a static equality filter applied to the child query. It makes
	// POLYMORPHIC children addressable: a shared Attachment/Address table with
	// an `owner_model` discriminator declares Scope {"owner_model":"Customer"}
	// alongside ForeignKey "owner_id". Empty = no extra filter. Mirrors
	// manifest.RelationDef.Scope so the value survives the manifest → host hop.
	Scope map[string]string `json:"scope,omitempty"`

	// Label is an optional i18n key / human label for the related-records panel.
	Label string `json:"label,omitempty"`

	// Readonly declares that the relation panel must not allow create/edit/delete
	// of child rows (e.g. an append-only ledger like an inventory kardex).
	// Mirrors manifest.RelationDef.Readonly so the value survives the
	// manifest → host hop. Pure UI metadata; defaults false.
	Readonly bool `json:"readonly,omitempty"`
}

// HasRelations is implemented by compiled models that declare model-to-model
// edges at the metadata layer. Implementations are optional — models without
// relations simply omit it. The metadata service reads this to auto-derive
// ColumnDef.Ref values, so a model that declares
//
//	{Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"}
//
// gets `Ref="customers"` stamped onto its `customer_id` column without the
// author repeating the target name on every column declaration.
type HasRelations interface {
	DefineRelations() []RelationDef
}
