package manifest

// Lite returns a shell-oriented copy of m: identity, frontend, navigation and
// a slim model_definitions index (model_key/table_name/label only). Heavy
// blocks (columns, actions, hooks, i18n, connectors, documents, …) are dropped
// so GET /metacore/manifests?lite=1 stays small with large fleets.
//
// The host shell needs the slim model index to resolve v3 nav `{model}` →
// `/m/<table>` and frontend.load policy; it does NOT need column schemas
// (those arrive via /metadata/table/:model on demand).
func Lite(m Manifest) Manifest {
	out := m
	out.Actions = nil
	out.Hooks = nil
	out.LifecycleHooks = nil
	out.Tools = nil
	out.Capabilities = nil
	out.Permissions = nil
	out.I18n = nil
	out.Settings = nil
	out.Extensions = nil
	out.Events = nil
	out.Signature = nil
	out.Routes = nil
	out.Connectors = nil
	out.Schedules = nil
	out.Webhooks = nil
	out.EdgeDevices = nil
	out.Documents = nil
	out.Screenshots = nil
	out.Features = nil
	out.Readme = ""
	out.MetadataI18n = nil

	if m.Backend != nil {
		out.Backend = &BackendSpec{
			Runtime: m.Backend.Runtime,
			Entry:   m.Backend.Entry,
		}
	}

	if len(m.ModelDefinitions) > 0 {
		slim := make([]ModelDefinition, len(m.ModelDefinitions))
		for i, d := range m.ModelDefinitions {
			slim[i] = ModelDefinition{
				TableName:  d.TableName,
				ModelKey:   d.ModelKey,
				Label:      d.Label,
				OrgScoped:  d.OrgScoped,
				SoftDelete: d.SoftDelete,
			}
		}
		out.ModelDefinitions = slim
	}
	return out
}

// LiteAll maps Lite over a slice.
func LiteAll(in []Manifest) []Manifest {
	if len(in) == 0 {
		return in
	}
	out := make([]Manifest, len(in))
	for i := range in {
		out[i] = Lite(in[i])
	}
	return out
}
