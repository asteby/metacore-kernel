package manifest

import (
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// kernelRequirementKey is the reserved compatibility.requires[] key whose
// version range the kernel reads as the legacy `Manifest.Kernel` field.
const kernelRequirementKey = "kernel"

// managedColumns are the physical columns the kernel's dynamic schema layer
// injects automatically for every addon table (see dynamic.CreateTable). A v3
// model declares these explicitly to document its full physical shape, but the
// legacy ModelDefinition path must NOT re-declare them or CreateTable would
// emit duplicate column definitions ("id" twice, etc.). We strip them during
// mapping and instead translate their presence into the legacy flags
// (organization_id → OrgScoped, deleted_at → SoftDelete).
var managedColumns = map[string]struct{}{
	"id":         {},
	"created_at": {},
	"updated_at": {},
}

// FromV3 maps a validated v3 manifest into the legacy manifest.Manifest shape
// so the ~14 existing kernel consumers (installer, lifecycle, bridge, dynamic
// hooks, host, httpx/metacore, …) keep working unchanged during the 3.x
// dual-read window. It is intentionally conservative: fields with a clean v3
// equivalent are populated; fields with no faithful mapping (or that no kernel
// consumer reads) are left zero-valued rather than fabricated.
//
// The direction is strictly one-way: the manifest package imports manifest/v3,
// never the reverse, so the v3 contract stays free of legacy coupling.
func FromV3(m *v3.Manifest) Manifest {
	if m == nil {
		return Manifest{}
	}

	out := Manifest{
		Key:          m.Metadata.Key,
		Name:         m.Metadata.Name,
		Description:  m.Metadata.Description,
		Version:      m.Metadata.Version,
		Category:     m.Metadata.Category,
		Author:       m.Metadata.Author,
		Website:      m.Metadata.Website,
		License:      m.Metadata.License,
		Readme:       m.Metadata.Readme,
		Screenshots:  m.Metadata.Screenshots,
		Features:     m.Metadata.Features,
		Countries:    m.Metadata.Countries,
		MetadataI18n: mapMetadataI18n(m.Metadata.I18n),
	}

	// Icon triplet. v3 carries the structured {type, slug, color}; the legacy
	// single-string Icon stays empty (consumers prefer the triplet).
	if m.Metadata.Icon != nil {
		out.IconType = m.Metadata.Icon.Type
		out.IconSlug = m.Metadata.Icon.Slug
		out.IconColor = m.Metadata.Icon.Color
	}

	// Kernel semver range lives under compatibility.requires[key=="kernel"].
	for _, r := range m.Compatibility.Requires {
		if r.Key == kernelRequirementKey {
			out.Kernel = r.Version
			break
		}
	}

	// Tenancy → TenantIsolation. nil tenancy maps to "" which the kernel and
	// dynamic.ParseIsolation both treat as the "shared" default.
	if m.Tenancy != nil {
		out.TenantIsolation = m.Tenancy.Isolation
	}

	// Frontend federation block. v3.Frontend mirrors FrontendSpec 1:1, so the
	// mapping is a straight field copy.
	if m.Frontend != nil {
		out.Frontend = &FrontendSpec{
			Entry:     m.Frontend.Entry,
			Format:    m.Frontend.Format,
			Expose:    m.Frontend.Expose,
			Integrity: m.Frontend.Integrity,
			Container: m.Frontend.Container,
			Layout:    m.Frontend.Layout,
		}
	}

	out.Capabilities = mapCapabilities(m.Capabilities)
	out.ModelDefinitions = mapModels(m.Models)
	// Shared tenancy (default): every model is org-scoped at the host layer even
	// when the author omitted organization_id from columns[]. Ops DDL always
	// materializes the managed column; OrgScoped=false historically built a
	// reflect struct WITHOUT OrganizationID, and /api/data skipped the tenant
	// filter → cross-org list leak (e.g. POSSalePayment). Force the flag so
	// BuildStructType / IndexMe stay fail-closed. Authoring still SHOULD declare
	// the column (v3.Validate enforces it); this is the runtime backstop.
	if isSharedTenancy(m) {
		for i := range out.ModelDefinitions {
			out.ModelDefinitions[i].OrgScoped = true
		}
	}
	out.Navigation = mapNavigation(m)
	out.Settings = mapSettings(m.Settings)
	out.Actions = mapActions(m)
	out.Tools = mapTools(m)
	out.Events = mapEvents(m)
	out.LifecycleHooks = mapLifecycle(m.Lifecycle)
	out.I18n = mapI18n(m.I18n)
	out.Signature = mapSignature(m.Signature)
	out.Backend = deriveBackend(m)
	out.Connectors = mapConnectors(m.Connectors)
	out.Schedules = mapSchedules(m.Schedules)
	out.Webhooks = mapWebhooks(m.Webhooks)
	out.Documents = mapDocuments(m)

	return out
}

// isSharedTenancy reports whether the addon uses the default shared-schema
// isolation (single table set, rows distinguished by organization_id). nil
// tenancy and empty isolation both mean shared — matching ParseIsolation.
func isSharedTenancy(m *v3.Manifest) bool {
	if m == nil || m.Tenancy == nil || m.Tenancy.Isolation == "" {
		return true
	}
	return m.Tenancy.Isolation == "shared"
}

// mapDocuments folds v3 contributions.documents[] onto the host Manifest so the
// render engine can read the printable-document templates off the installed
// manifest. Near-1:1 field copy; no contributions or an empty slice maps to nil
// (the addon prints nothing, the back-compat default).
func mapDocuments(m *v3.Manifest) []DocumentDef {
	if m.Contributions == nil || len(m.Contributions.Documents) == 0 {
		return nil
	}
	out := make([]DocumentDef, 0, len(m.Contributions.Documents))
	for _, d := range m.Contributions.Documents {
		out = append(out, DocumentDef{
			Key:      d.Key,
			Model:    d.Model,
			Template: d.Template,
			Paper:    d.Paper,
			Filename: d.Filename,
		})
	}
	return out
}

// mapConnectors / mapSchedules / mapWebhooks fold the v3 pipeline-runtime
// primitives onto the host Manifest so the connectors runtime, the scheduler
// and the webhookin receiver can read them off the installed manifest. Each is
// a near-1:1 field copy; an empty input maps to nil (no primitive declared,
// the back-compat default).
func mapConnectors(in []v3.Connector) []ConnectorDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]ConnectorDef, 0, len(in))
	for _, c := range in {
		out = append(out, ConnectorDef{
			Key:         c.Key,
			Label:       c.Label,
			Auth:        c.Auth,
			Credentials: mapCredentials(c.Credentials),
			FormLayout:  c.FormLayout,
			TestExport:  c.TestExport,
		})
	}
	return out
}

// mapCredentials projects v3 connector credential Settings onto CredentialDefs.
// A credential whose type is "secret" sets Secret so the host stores it
// encrypted (mirroring SettingDef.Secret).
func mapCredentials(in []v3.Setting) []CredentialDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]CredentialDef, 0, len(in))
	for _, s := range in {
		out = append(out, CredentialDef{
			Key:           s.Key,
			Type:          s.Type,
			Default:       s.Default,
			Required:      s.Required,
			Validation:    s.Validation,
			Secret:        s.Type == "secret",
			OptionsSource: s.OptionsSource,
			Section:       s.Section,
		})
	}
	return out
}

func mapSchedules(in []v3.Schedule) []ScheduleDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]ScheduleDef, 0, len(in))
	for _, s := range in {
		out = append(out, ScheduleDef{Key: s.Key, Every: s.Every, Do: s.Do})
	}
	return out
}

func mapWebhooks(in []v3.InboundWebhook) []InboundWebhookDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]InboundWebhookDef, 0, len(in))
	for _, w := range in {
		out = append(out, InboundWebhookDef{
			Key:       w.Key,
			Path:      w.Path,
			Verify:    w.Verify,
			SecretRef: w.SecretRef,
			Do:        w.Do,
		})
	}
	return out
}

// mapCapabilities is a 1:1 field copy — both shapes are {Kind, Target, Reason}.
func mapCapabilities(in []v3.Capability) []Capability {
	if len(in) == 0 {
		return nil
	}
	out := make([]Capability, 0, len(in))
	for _, c := range in {
		out = append(out, Capability{Kind: c.Kind, Target: c.Target, Reason: c.Reason})
	}
	return out
}

// mapModels translates v3 Models into legacy ModelDefinitions. The kernel's
// dynamic.CreateTable auto-injects id/organization_id/created_at/updated_at/
// deleted_at, so those columns are stripped here and surfaced as OrgScoped /
// SoftDelete flags instead. v3 ForeignKeys (the physical FK constraints carried
// by the OWNING column) still do not map onto legacy owner-rooted RelationDef
// shapes and are not guessed; the addon-declared inverse-view relations
// (m.Relations) ARE mapped, via mapModelRelations, so a detail page can list a
// model's child records.
func mapModels(in []v3.Model) []ModelDefinition {
	if len(in) == 0 {
		return nil
	}
	out := make([]ModelDefinition, 0, len(in))
	for _, m := range in {
		def := ModelDefinition{
			TableName: m.Table,
			ModelKey:  m.Key,
			Label:     m.Label,
		}
		// Index of single-column index declarations so we can fold the
		// unique/index hint back onto the matching ColumnDef.
		uniqueCols := map[string]struct{}{}
		indexCols := map[string]struct{}{}
		for _, idx := range m.Indices {
			if len(idx.Columns) != 1 {
				continue // composite indices have no ColumnDef equivalent.
			}
			if idx.Unique {
				uniqueCols[idx.Columns[0]] = struct{}{}
			} else {
				indexCols[idx.Columns[0]] = struct{}{}
			}
		}
		// Index of single-column foreign_keys so we can DERIVE a column ref from
		// the FK's target model. A model commonly declares its relations at the
		// MODEL level (foreign_keys: [{columns:["product_id"], references:{model:
		// "inventory.Product"}}]) and leaves the column's own `ref` empty. The
		// host resolves relation display names ONLY from a column's ref, so such
		// FK columns would render the raw UUID instead of the related record's
		// name. Auto-setting Ref to references.model here (when the column carries
		// no explicit ref) makes every model-level FK render as a relation picker
		// + resolved name with zero per-addon wiring. An author-provided column
		// ref always wins (we only fill the gap). Composite FKs have no single
		// ColumnDef equivalent and are skipped.
		fkRef := map[string]string{}
		for _, fk := range m.ForeignKeys {
			if len(fk.Columns) != 1 || fk.References.Model == "" {
				continue
			}
			fkRef[fk.Columns[0]] = fk.References.Model
		}
		for _, c := range m.Columns {
			switch c.Name {
			case "organization_id":
				def.OrgScoped = true
				continue
			case "deleted_at":
				def.SoftDelete = true
				continue
			}
			if _, managed := managedColumns[c.Name]; managed {
				continue
			}
			// v3 Column.Comment has no legacy ColumnDef slot, so it
			// intentionally does NOT round-trip — consumers read it off the
			// v3-served metadata. We do not grow legacy ColumnDef for it.
			col := ColumnDef{
				Name:     c.Name,
				Type:     c.Type,
				Required: c.NotNull,
				Default:  renderColumnDefault(c.Default),
				// Label (a literal header or an i18n key like "models.x.y")
				// rides across so the derived metadata keeps the author's
				// header and the localized transformer can resolve a key at
				// serve time instead of falling back to the humanized name.
				Label: c.Label,
				// Display hints are pure UI metadata: they ride the legacy
				// ColumnDef as a carrier so they survive the v3 → host
				// conversion and land on the served modelbase.ColumnDef. They
				// never touch the DDL plane.
				CellStyle:   c.Display,
				StyleConfig: c.DisplayConfig,
				Tooltip:     c.Tooltip,
				Description: c.Description,
				// Widget is the FORM input plane (image/upload/textarea/…). It
				// rides the legacy ColumnDef.Widget so DeriveFormFields renders
				// the declared picker instead of inferring one. Pure UI metadata.
				Widget: c.Widget,
				// Ref turns the column into a dynamic_select (FK picker): it
				// rides the legacy ColumnDef.Ref so DeriveTableColumns /
				// DeriveFormFields project it onto the served modelbase Ref and
				// the SDK renders a searchable relation picker — without a
				// belongs_to relation or a custom action. Pure UI metadata.
				Ref: c.Ref,
				// OptionsSource is the DYNAMIC twin of Options: a provider key
				// (e.g. "registered_models") the HOST resolves at serve time to
				// materialise localized options. It rides the legacy
				// ColumnDef.OptionsSource so the key survives the v3 → host
				// conversion; the kernel implements no providers.
				OptionsSource: c.OptionsSource,
				// DependsOn names a sibling column whose value scopes this
				// dependent picker (cascade filter_value). Rides the legacy
				// ColumnDef so the SDK re-fetches on change.
				DependsOn: c.DependsOn,
				// Scan opts the column's form input into camera barcode scanning;
				// rides ColumnDef.Scan so DeriveFormFields carries it onto the
				// served FieldDef and the SDK shows a scan-to-fill button.
				Scan: c.Scan,
				// Readonly rides through so DeriveFormFields excludes the
				// system-generated column from create and marks it read-only in edit.
				Readonly: c.Readonly,
				// Constraints ride through so the dynamic engine evaluates the
				// declarative guard predicates inside the create/update transaction.
				Constraints: mapColumnConstraints(c.Constraints),
				// Sequence binds the column to a model folio counter (auto-stamped
				// on create).
				Sequence: c.Sequence,
				// Generated rides through so the DDL builder emits the column as
				// `GENERATED ALWAYS AS (<expr>) STORED` (Postgres-maintained).
				Generated: c.Generated,
				// VisibleWhen rides through so form derivation projects the
				// conditional-visibility predicate onto the served FieldDef and the
				// SDK shows/hides the field against the live form values. Pure UI.
				VisibleWhen: mapVisibleWhen(c.VisibleWhen),
				// Section rides through so form derivation projects the field's
				// form_layout section/step key onto the served FieldDef. Pure UI.
				Section: c.Section,
			}
			// Options is EITHER the STATIC-select list (array form) OR the
			// DYNAMIC dependent-source object (DynamicOptions). The static list
			// (with optional icon/color/image visuals) rides ColumnDef.Options;
			// the dynamic object rides ColumnDef.OptionsConfig so the
			// {source,filter_by,value,label_ref,description} block survives to the
			// served modelbase.FieldOptionsConfig.
			for _, o := range c.Options.Static {
				col.Options = append(col.Options, Option{
					Value: o.Value,
					Label: o.Label,
					Icon:  o.Icon,
					Color: o.Color,
					Image: o.Image,
					When:  mapOptionCondition(o.When),
				})
			}
			if d := c.Options.Dynamic; d != nil {
				col.OptionsConfig = &DynamicOptionsDef{
					Type:        "dynamic",
					Source:      d.Source,
					FilterBy:    d.FilterBy,
					Value:       d.Value,
					Label:       d.Label,
					LabelRef:    d.LabelRef,
					Description: d.Description,
				}
			}
			// A base_path inside display_config is also projected onto the
			// dedicated BasePath slot the SDK reads for URL/route prefixes.
			if bp, ok := c.DisplayConfig["base_path"].(string); ok {
				col.BasePath = bp
			}
			if _, ok := uniqueCols[c.Name]; ok {
				col.Unique = true
			}
			if _, ok := indexCols[c.Name]; ok {
				col.Index = true
			}
			// Derive the column ref from a model-level foreign_keys entry when the
			// author did not state one explicitly (an explicit c.Ref above wins).
			if col.Ref == "" {
				if target, ok := fkRef[c.Name]; ok {
					col.Ref = target
				}
			}
			def.Columns = append(def.Columns, col)
		}
		def.Relations = mapModelRelations(m.Relations)
		def.Seed = mapModelSeed(m.Seed)
		def.Formulas = mapModelFormulas(m.Formulas)
		// Stage machine: the field/stages/transitions/hooks ride across so the
		// dynamic engine derives the status display, validates moves and fires
		// on_transition hooks. Empty StageField/Stages leaves the model
		// unrestricted (the legacy behaviour).
		def.StageField = m.StageField
		def.Stages = mapModelStages(m.Stages)
		def.Transitions = mapModelTransitions(m.Transitions)
		def.OnTransition = mapModelTransitionHooks(m.OnTransition)
		def.Locking = m.Locking
		def.Sequences = mapModelSequences(m.Sequences)
		// FormLayout rides through so the host projects the create/edit form
		// grouping (collapsible sections or step wizard) onto the served metadata.
		// Nil = a flat form (legacy). Pure UI.
		def.FormLayout = mapFormLayout(m.FormLayout)
		out = append(out, def)
	}
	return out
}

// mapModelRelations folds v3 ModelRelations (the inverse 1:N / N:M view edges
// an addon declares for its detail pages) onto the legacy ModelDefinition's
// Relations slice so they survive the v3 → host conversion. The host projects
// them into modelbase.TableMetadata.Relations. v3 ModelRelation does not carry
// References/Pivot (the inverse-view contract only needs Through + ForeignKey
// + an optional polymorphic Scope), so those legacy fields stay zero. Scope and
// Label copy across verbatim.
func mapModelRelations(in []v3.ModelRelation) []RelationDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]RelationDef, 0, len(in))
	for _, r := range in {
		out = append(out, RelationDef{
			Name:       r.Name,
			Kind:       r.Kind,
			Through:    r.Through,
			ForeignKey: r.ForeignKey,
			Scope:      r.Scope,
			Label:      r.Label,
			Readonly:   r.Readonly,
			Embed:      r.Embed,
			Rollups:    mapRollups(r.Rollups),
		})
	}
	return out
}

// mapRollups folds a v3 relation's rollups (Tier-1 parent-aggregate specs)
// onto the legacy RelationDef.Rollups slice so they survive the v3 → host
// conversion. The dynamic compute engine reads them off the legacy shape at
// install time. A nil/empty input maps to nil (the common case).
func mapRollups(in []v3.Rollup) []Rollup {
	if len(in) == 0 {
		return nil
	}
	out := make([]Rollup, 0, len(in))
	for _, r := range in {
		out = append(out, Rollup{
			Target: r.Target,
			Fn:     r.Fn,
			From:   r.From,
			Expr:   r.Expr,
		})
	}
	return out
}

// mapModelFormulas folds a v3 model's formulas (Tier-2 same-row computed
// columns) onto the legacy ModelDefinition.Formulas slice so they survive the
// v3 → host conversion. The dynamic compute engine reads them off the legacy
// shape at install time. A nil/empty input maps to nil (the common case).
func mapModelFormulas(in []v3.Formula) []Formula {
	if len(in) == 0 {
		return nil
	}
	out := make([]Formula, 0, len(in))
	for _, f := range in {
		out = append(out, Formula{
			Target:  f.Target,
			Expr:    f.Expr,
			Tier:    f.Tier,
			Handler: f.Handler,
		})
	}
	return out
}

// mapColumnConstraints folds a v3 column's declarative guard predicates onto
// the legacy ColumnDef.Constraints slice so the dynamic engine can evaluate them
// inside the create/update transaction. Nil/empty maps to nil (the common case).
func mapColumnConstraints(in []v3.Constraint) []ConstraintDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]ConstraintDef, 0, len(in))
	for _, c := range in {
		out = append(out, ConstraintDef{Expr: c.Expr, ErrorKey: c.ErrorKey})
	}
	return out
}

// mapModelSequences folds a v3 model's folio counters onto the legacy
// ModelDefinition.Sequences slice. Nil/empty maps to nil (the common case).
func mapModelSequences(in []v3.Sequence) []SequenceDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]SequenceDef, 0, len(in))
	for _, s := range in {
		out = append(out, SequenceDef{Key: s.Key, Scope: s.Scope, Format: s.Format})
	}
	return out
}

// mapModelStages / mapModelTransitions / mapModelTransitionHooks fold a v3
// model's stage machine onto the host ModelDefinition so the dynamic engine can
// derive the status display, validate stage moves and fire on_transition hooks.
// Each is a 1:1 field copy; an empty input maps to nil (the no-stage-machine
// default that leaves the model unrestricted).
func mapModelStages(in []v3.Stage) []StageDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]StageDef, 0, len(in))
	for _, s := range in {
		out = append(out, StageDef{
			Key:     s.Key,
			Label:   s.Label,
			Color:   s.Color,
			Order:   s.Order,
			IsFinal: s.IsFinal,
		})
	}
	return out
}

func mapModelTransitions(in []v3.Transition) []TransitionDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]TransitionDef, 0, len(in))
	for _, t := range in {
		out = append(out, TransitionDef{From: t.From, To: t.To})
	}
	return out
}

func mapModelTransitionHooks(in []v3.TransitionHook) []TransitionHookDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]TransitionHookDef, 0, len(in))
	for _, h := range in {
		out = append(out, TransitionHookDef{
			From:     h.From,
			To:       h.To,
			Set:      h.Set,
			Do:       h.Do,
			Required: h.Required,
		})
	}
	return out
}

// mapModelSeed folds a v3 model's seed block (declarative default data the
// installer inserts on install, idempotent by a natural key column) onto the
// host ModelDefinition.Seed so it survives the v3 → host conversion. The host
// (ops executor) reads def.Seed to perform the seeding. Key and Rows copy
// across verbatim; a nil v3 seed maps to a nil host seed (the common case).
func mapModelSeed(in *v3.Seed) *SeedDef {
	if in == nil {
		return nil
	}
	return &SeedDef{
		Key:  in.Key,
		Rows: in.Rows,
	}
}

// renderColumnDefault translates a v3 column default into a DDL-safe legacy
// ColumnDef.Default. In v3 a column `default` is a JSON value, so a bare string
// like "draft", "MXN" or "{}" is a literal VALUE — not a SQL expression — and
// must be emitted as a quoted SQL literal ('draft') to satisfy the DDL default
// whitelist. Values that already read as valid SQL (numeric, an existing
// quoted literal, or a recognised function/keyword like now() /
// gen_random_uuid() / true / false / null) pass through untouched. Numbers,
// bools and nil are handled by DefaultLiteral downstream and pass through here.
func renderColumnDefault(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if s == "" {
		return nil
	}
	if _, valid := DefaultLiteral(s); valid {
		return s
	}
	// Quote the bare literal, unless it carries characters the whitelist's
	// quoted-string form forbids (' " ; \) — leave those for validation to flag
	// rather than emit an unsafe DEFAULT expression.
	if !strings.ContainsAny(s, "'\";\\") {
		return "'" + s + "'"
	}
	return s
}

// mapNavigation copies contributions.navigation field-by-field. NavGroup and
// NavItem have the same shape in both contracts.
//
// It also RESOLVES each model-bound nav item's URL to the host dynamic-CRUD
// route (/m/<table_name>) up front, using the manifest's own models. Addon nav
// items typically declare only `model: "Customer"` (no url); without resolution
// the consumer must rebuild a model→table index from model_definitions (which
// the frontend often lacks → every item falls back to "#" and clicking does
// nothing). Resolving here makes the served nav self-contained.
func mapNavigation(m *v3.Manifest) []NavGroup {
	if m.Contributions == nil || len(m.Contributions.Navigation) == 0 {
		return nil
	}
	// model key → table name, the route the kernel also creates tables at.
	modelTable := make(map[string]string, len(m.Models))
	for _, mod := range m.Models {
		if mod.Key != "" && mod.Table != "" {
			modelTable[mod.Key] = mod.Table
		}
	}
	out := make([]NavGroup, 0, len(m.Contributions.Navigation))
	for _, g := range m.Contributions.Navigation {
		out = append(out, NavGroup{
			Title:  g.Title,
			Icon:   g.Icon,
			Target: g.Target,
			Items:  mapNavItems(g.Items, modelTable),
		})
	}
	return out
}

func mapNavItems(in []v3.NavItem, modelTable map[string]string) []NavItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]NavItem, 0, len(in))
	for _, it := range in {
		url := it.URL
		if url == "" && it.Model != "" {
			if table, ok := modelTable[it.Model]; ok {
				url = "/m/" + table
			}
		}
		out = append(out, NavItem{
			Title:      it.Title,
			URL:        url,
			Icon:       it.Icon,
			Model:      it.Model,
			Permission: it.Permission,
			Items:      mapNavItems(it.Items, modelTable),
			Filter:     it.Filter,
			// Kanban view-type hint rides across so the host can project it onto
			// the served TableMetadata and the SDK picks the board renderer.
			ViewType: it.ViewType,
			GroupBy:  it.GroupBy,
			// Screen → API cap deps for host RBAC expand (Terminal Acceder, etc.).
			RequiresCapabilities: it.RequiresCapabilities,
		})
	}
	return out
}

// mapSettings maps v3 settings into legacy SettingDef. v3 {key,type,label,
// default,options} → legacy {Key,Type,Label,DefaultValue,Options}. v3 has no
// Secret flag, so legacy Secret stays false.
//
// v3 Setting.Description has no legacy SettingDef slot, so it intentionally does
// NOT round-trip here — consumers read it off the v3-served metadata. We do not
// grow legacy SettingDef for it. (v3 Setting.Type "number" likewise carries
// across as the raw type string; no special-casing is needed.)
func mapSettings(in []v3.Setting) []SettingDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]SettingDef, 0, len(in))
	for _, s := range in {
		def := SettingDef{
			Key:          s.Key,
			Label:        s.Label,
			Type:         s.Type,
			DefaultValue: s.Default,
		}
		for _, o := range s.Options {
			def.Options = append(def.Options, Option{Value: o.Value, Label: o.Label})
		}
		out = append(out, def)
	}
	return out
}

// mapActions projects contributions.actions into the legacy Actions map keyed
// by target model (the same "<model>::<action>" addressing the bridge expects).
// Actions with no target model are bucketed under "" so they still register.
//
// Besides the Handler→Trigger / TargetModel dispatch core, the richer v3 Action
// surface (declarative action modals + custom federated modals) is carried over:
// Icon, Confirm, ConfirmMessage and Modal copy straight across, and each v3
// ActionField is folded into a legacy FieldDef.
//
// The legacy FieldDef carries matching JSON tags for the rich ActionField
// properties (widget / ref / placeholder / search_endpoint / item_fields /
// total / balance and the upload triplet accept / max_size / storage_path), so
// those survive the v3 → host conversion and the SDK renders the full
// declarative modal off the manifest-served action metadata. See
// mapActionFields for the per-field copy.
func mapActions(m *v3.Manifest) map[string][]ActionDef {
	if m.Contributions == nil || len(m.Contributions.Actions) == 0 {
		return nil
	}
	out := map[string][]ActionDef{}
	for _, a := range m.Contributions.Actions {
		def := ActionDef{
			Key:            a.Key,
			Name:           a.Key,
			Label:          a.Label,
			Icon:           a.Icon,
			Confirm:        a.Confirm,
			ConfirmMessage: a.ConfirmMessage,
			Modal:          a.Modal,
			Placement:      a.Placement,
			ModalWidth:     a.ModalWidth,
			RequiresState:  a.RequiresState,
			Fields:         mapActionFields(a.Fields),
			Steps:          mapActionSteps(a.Steps),
		}
		switch a.Handler.Type {
		case "wasm":
			def.Trigger = &ActionTrigger{Type: "wasm", Export: a.Handler.Function}
		case "webhook":
			def.Trigger = &ActionTrigger{Type: "webhook"}
		case "connector":
			// Cross-addon dispatch: the export runs in the connector-owning addon.
			def.Trigger = &ActionTrigger{Type: "connector", Connector: a.Handler.Connector, Export: a.Handler.Export}
		}
		if a.Idempotency != nil {
			def.Idempotency = &IdempotencyDef{KeyField: a.Idempotency.KeyField}
		}
		out[a.TargetModel] = append(out[a.TargetModel], def)
	}
	return out
}

// mapActionSteps folds a v3 action's wizard pages onto ActionStepDefs, each
// page's fields through the same per-field copy as the flat form.
func mapActionSteps(in []v3.ActionStep) []ActionStepDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]ActionStepDef, 0, len(in))
	for _, st := range in {
		out = append(out, ActionStepDef{
			Title:       st.Title,
			Description: st.Description,
			Fields:      mapActionFields(st.Fields),
		})
	}
	return out
}

// mapActionFields folds v3 ActionFields into FieldDefs the host serves to the
// SDK. v3 field.Key maps to BOTH FieldDef.Key (the tag the host/SDK read) and
// FieldDef.Name (kept for legacy consumers); Label/Type/Required/Default and
// mapOptionCondition projects a v3 static-option cascade guard onto the legacy
// carrier so it survives the v3 → host conversion and lands on the served
// modelbase.OptionDef.When. Nil stays nil (retro-compatible: no `when` block).
func mapOptionCondition(w *v3.OptionCondition) *OptionCondition {
	if w == nil {
		return nil
	}
	return &OptionCondition{
		Field: w.Field,
		In:    w.In,
		NotIn: w.NotIn,
	}
}

// mapVisibleWhen projects a v3 conditional-visibility predicate onto the legacy
// carrier so the {field, equals, in} block survives the v3 → host conversion
// and lands on the served modelbase FieldDef/ColumnDef.VisibleWhen. Nil stays
// nil (retro-compatible: no `visible_when` block = always visible).
func mapVisibleWhen(w *v3.VisibleWhen) *VisibleWhenDef {
	if w == nil {
		return nil
	}
	return &VisibleWhenDef{
		Field:  w.Field,
		Equals: w.Equals,
		In:     w.In,
	}
}

// mapImportSpec projects a v3 model's `import` block onto the legacy carrier
// the host converts into a modelbase.ImportSpec. Nil in, nil out: a model that
// declares nothing keeps the derived-spec behaviour.
func mapImportSpec(in *v3.ImportSpec) *ImportSpecDef {
	if in == nil {
		return nil
	}
	out := &ImportSpecDef{
		MaxRows:      in.MaxRows,
		SheetName:    in.SheetName,
		Instructions: append([]string(nil), in.Instructions...),
		Columns:      make([]ImportColumnDef, 0, len(in.Columns)),
	}
	for _, col := range in.Columns {
		out.Columns = append(out.Columns, ImportColumnDef{
			Key:       col.Key,
			Header:    col.Header,
			Aliases:   append([]string(nil), col.Aliases...),
			Required:  col.Required,
			Type:      col.Type,
			Example:   col.Example,
			Hint:      col.Hint,
			Generator: col.Generator,
			Transform: col.Transform,
		})
	}
	return out
}

// mapFormLayout projects a v3 model's create/edit form grouping onto the legacy
// carrier so the {mode, sections} block survives the v3 → host conversion and
// lands on the served form metadata. Nil stays nil (a flat form). Section
// titles/descriptions copy across verbatim (literal or i18n key).
func mapFormLayout(fl *v3.FormLayout) *FormLayoutDef {
	if fl == nil {
		return nil
	}
	out := &FormLayoutDef{Mode: fl.Mode}
	for _, s := range fl.Sections {
		out.Sections = append(out.Sections, FormSectionDef{
			Key:         s.Key,
			Title:       s.Title,
			Description: s.Description,
			Collapsed:   s.Collapsed,
			VisibleWhen: mapVisibleWhen(s.VisibleWhen),
		})
	}
	return out
}

// FieldOptions copy across.
//
// The rich properties — widget, ref, placeholder, search_endpoint, total,
// balance, and the nested item_fields of a line-items (type:"array") group —
// are now forwarded too. FieldDef carries matching JSON tags, so they survive
// the v3 → host conversion and the SDK (runtime-react) renders the full
// declarative modal (searchable pickers, multi-column line-items with
// totals/balance). Previously these were dropped, collapsing every rich field
// to a plain input. item_fields recurses (each item column is itself an
// ActionField).
func mapActionFields(in []v3.ActionField) []FieldDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]FieldDef, 0, len(in))
	for _, f := range in {
		fd := FieldDef{
			Name:     f.Key,
			Key:      f.Key, // host/SDK key off "key"; Name kept for legacy
			Label:    f.Label,
			Type:     f.Type,
			Required: f.Required,
			Default:  f.Default,
			// Rich properties — forwarded so the declarative modal renders
			// searchable pickers and line-items grids instead of plain inputs.
			Widget:         f.Widget,
			Ref:            f.Ref,
			Placeholder:    f.Placeholder,
			SearchEndpoint: f.SearchEndpoint,
			Total:          f.Total,
			// Scan opts the field into camera barcode scan-to-fill (text/number
			// input or dynamic_select reference). Forwarded to modelbase.FieldDef.
			Scan: f.Scan,
			// Upload-field properties (type:"upload") — forwarded so the host's
			// upload widget gets the accept allow-list, byte cap and storage prefix.
			Accept:      f.Accept,
			MaxSize:     f.MaxSize,
			StoragePath: f.StoragePath,
			// Remote-model visual mapping for dynamic_select — forwarded so the
			// SDK can render a thumbnail/icon/colour beside each option's label.
			LabelImage: f.LabelImage,
			LabelIcon:  f.LabelIcon,
			LabelColor: f.LabelColor,
			// ItemFields are themselves ActionFields — recurse so a line-items
			// group's columns (debit/credit/account picker) survive intact.
			ItemFields: mapActionFields(f.ItemFields),
			// LockRows forwards the fixed-rows flag so the SDK hides the
			// add-row/delete-row controls on a prefilled line-items grid.
			LockRows: f.LockRows,
			// DependsOn forwards the cascade dependency so the SDK scopes +
			// re-fetches this picker's options from the depended-on field's value.
			DependsOn: f.DependsOn,
			// OptionsSource forwards the host-registered dynamic options provider
			// key (e.g. "connector_repos") so the host materialises the field's
			// choices from its registry at metadata-serve time.
			OptionsSource: f.OptionsSource,
			// VisibleWhen forwards the conditional-visibility predicate so the SDK
			// shows/hides this action field against the live form values.
			VisibleWhen: mapVisibleWhen(f.VisibleWhen),
		}
		// Options is EITHER the static value/label list (array form) OR the
		// dynamic dependent-source object (DynamicOptions). The static list rides
		// FieldDef.Options; the dynamic object rides FieldDef.OptionsConfig so the
		// {source,filter_by,value,label_ref,description} block reaches the served
		// action spec and the host's OptionsConfigResolver.
		for _, o := range f.Options.Static {
			// Static-option visual hints (icon/color/image) ride across so a
			// status/brand option list renders richly in the SDK.
			fd.Options = append(fd.Options, Option{
				Value: o.Value,
				Label: o.Label,
				Icon:  o.Icon,
				Color: o.Color,
				Image: o.Image,
				When:  mapOptionCondition(o.When),
			})
		}
		if d := f.Options.Dynamic; d != nil {
			fd.OptionsConfig = &DynamicOptionsDef{
				Type:        "dynamic",
				Source:      d.Source,
				FilterBy:    d.FilterBy,
				Value:       d.Value,
				Label:       d.Label,
				LabelRef:    d.LabelRef,
				Description: d.Description,
			}
		}
		if f.Balance != nil {
			fd.Balance = &FieldBalanceRule{
				DebitColumn:    f.Balance.DebitColumn,
				CreditColumn:   f.Balance.CreditColumn,
				Message:        f.Balance.Message,
				RequireNonzero: f.Balance.RequireNonzero,
			}
		}
		out = append(out, fd)
	}
	return out
}

// mapTools projects contributions.tools into legacy ToolDef. v3 tools are leaner
// ({key, description, input_schema, handler}) than the rich legacy ToolDef; only
// the fields with a clean equivalent are populated.
func mapTools(m *v3.Manifest) []ToolDef {
	if m.Contributions == nil || len(m.Contributions.Tools) == 0 {
		return nil
	}
	out := make([]ToolDef, 0, len(m.Contributions.Tools))
	for _, t := range m.Contributions.Tools {
		td := ToolDef{
			ID:          t.Key,
			Name:        t.Key,
			Description: t.Description,
		}
		if t.Handler.Type == "webhook" {
			td.Endpoint = t.Handler.URL
		}
		out = append(out, td)
	}
	return out
}

// mapEvents derives the legacy Events list from the events this addon publishes
// (extension_points.events) plus its event:emit capabilities. Both are sources
// of truth in v3; the union keeps the legacy advisory drift-check happy.
func mapEvents(m *v3.Manifest) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if m.ExtensionPoints != nil {
		for _, e := range m.ExtensionPoints.Events {
			add(e.Name)
		}
	}
	for _, c := range m.Capabilities {
		if c.Kind == "event:emit" {
			add(c.Target)
		}
	}
	return out
}

// mapLifecycle translates the v3 Lifecycle block into the legacy
// LifecycleHooks map. v3 declares a single handler function name per
// transition (install/uninstall/enable/disable) dispatched against the addon's
// wasm module; the legacy map keys those transitions to a HookDef list with a
// wasm HookTarget. The v3 upgrade ladder (UpgradeStep[]) has no single-target
// legacy equivalent and is left unmapped (the installer's upgrade path reads
// migrations, not these hooks).
func mapLifecycle(lc *v3.Lifecycle) map[string][]HookDef {
	if lc == nil {
		return nil
	}
	out := map[string][]HookDef{}
	add := func(event, fn string) {
		if fn == "" {
			return
		}
		out[event] = append(out[event], HookDef{
			Event:  event,
			Target: HookTarget{Type: "wasm", Function: fn},
		})
	}
	add("install", lc.Install)
	add("uninstall", lc.Uninstall)
	add("enable", lc.Enable)
	add("disable", lc.Disable)
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapI18n maps the v3 i18n bundle pointer into the legacy lang-keyed map. The
// v3 bundles reference file PATHS only — the actual translation strings live in
// the bundle files and are loaded at runtime, NOT carried in the manifest. So
// we produce a map keyed by locale with empty inner maps purely so length /
// presence checks on Manifest.I18n behave, without fabricating any translation
// content.
func mapI18n(i *v3.I18n) map[string]map[string]string {
	if i == nil || len(i.Bundles) == 0 {
		return nil
	}
	out := make(map[string]map[string]string, len(i.Bundles))
	for _, b := range i.Bundles {
		if b.Locale == "" {
			continue
		}
		out[b.Locale] = map[string]string{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapMetadataI18n copies the v3 metadata.i18n catalog localizations into the
// internal map. Unlike mapI18n (app-UI bundle pointers), these carry the actual
// localized name/description/features inline, so the hub can store + serve them
// without reading any bundle file.
func mapMetadataI18n(in map[string]v3.MetadataLocale) map[string]MetadataLocale {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]MetadataLocale, len(in))
	for locale, l := range in {
		if locale == "" {
			continue
		}
		out[locale] = MetadataLocale{
			Name:        l.Name,
			Description: l.Description,
			Features:    l.Features,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// deriveBackend synthesises a BackendSpec from the handlers a v3 manifest
// declares. A v3 manifest cannot carry an explicit top-level "backend" block
// (the JSON schema is additionalProperties:false), so the backend spec must be
// derived from the wasm handler references embedded in contributions.actions[],
// contributions.subscriptions[], and lifecycle hooks.
//
// If ANY handler in those sources has type "wasm" a BackendSpec{Runtime:"wasm"}
// is returned with Exports set to the deduplicated set of function names
// declared by those handlers. The Entry defaults to the conventional path
// "backend/backend.wasm" when none of the handlers specify a URL (v3 Handler
// does not carry an Entry path; the bundle loader uses the default).
//
// Returns nil when no wasm handler is declared — the caller then leaves
// Manifest.Backend nil, which preserves the legacy webhook behaviour.
func deriveBackend(m *v3.Manifest) *BackendSpec {
	seen := map[string]struct{}{}
	var exports []string
	add := func(fn string) {
		if fn == "" {
			return
		}
		if _, ok := seen[fn]; ok {
			return
		}
		seen[fn] = struct{}{}
		exports = append(exports, fn)
	}

	if m.Contributions != nil {
		for _, a := range m.Contributions.Actions {
			if a.Handler.Type == "wasm" {
				add(a.Handler.Function)
			}
		}
		for _, s := range m.Contributions.Subscriptions {
			if s.Handler.Type == "wasm" {
				add(s.Handler.Function)
			}
		}
	}

	// Connector lookups/health-checks: a credential's options_source (populates
	// a dynamic_select at config time) and a connector's test_export ("test
	// connection") are wasm exports the host invokes, so they must be in the
	// whitelist the runtime dispatches against (runtime/wasm invokeImpl).
	for _, c := range m.Connectors {
		add(c.TestExport)
		for _, cr := range c.Credentials {
			if cr.Type == "dynamic_select" {
				add(cr.OptionsSource)
			}
		}
	}

	// Lifecycle hooks declared with wasm function names also contribute
	// to the export list so the wasm host can resolve them at dispatch time.
	if m.Lifecycle != nil {
		for _, fn := range []string{
			m.Lifecycle.Install,
			m.Lifecycle.Uninstall,
			m.Lifecycle.Enable,
			m.Lifecycle.Disable,
		} {
			// Lifecycle fields hold just a function name (no type field).
			// Only add them when there is already a wasm backend implied by
			// contributions, to avoid promoting a pure-webhook addon.
			if fn != "" && len(seen) > 0 {
				add(fn)
			}
		}
	}

	if len(exports) == 0 {
		return nil
	}
	return &BackendSpec{
		Runtime: "wasm",
		Entry:   "backend/backend.wasm",
		Exports: exports,
	}
}

// mapSignature maps the v3 detached-signature block into the legacy Signature.
// The shapes overlap only on Algorithm / Value / SignedAt; v3 has no developer
// identity / verified / digest / per-entry checksums, so those legacy fields
// stay zero (the security package treats an absent/empty Value as "unsigned").
func mapSignature(s *v3.Signature) *Signature {
	if s == nil {
		return nil
	}
	return &Signature{
		Algorithm: s.Algorithm,
		Value:     s.Value,
		SignedAt:  s.SignedAt,
	}
}
