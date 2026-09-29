package modelbase

import "sync"

// HasTranslations is optionally implemented by a model that ships its own
// message catalog next to its metadata: lang → key → text. Keys may be
// relative to the model ("table.title", "modal.fields.name") or full
// ("models.branches.table.title"); host.App.RegisterModel expands relative
// keys to "models.<registered key>.<key>" and merges the result into the
// app translator, so the "models.*" labels of DefineTable/DefineModal render
// localized without an app-wide bundle.
//
//	func (Branch) DefineTranslations() map[string]map[string]string {
//	    return map[string]map[string]string{
//	        "es": {"table.title": "Sucursales", "modal.fields.name": "Nombre"},
//	        "en": {"table.title": "Branches", "modal.fields.name": "Name"},
//	    }
//	}
type HasTranslations interface {
	DefineTranslations() map[string]map[string]string
}

var (
	translationsMu sync.RWMutex
	translations   = map[string][]map[string]map[string]string{}
)

// AddTranslations registers extra messages (lang → key → text) for the model
// registered under key, on top of its DefineTranslations. Later calls win on
// the same key. host.WithTranslations calls it.
func AddTranslations(key string, msgs map[string]map[string]string) {
	if key == "" || len(msgs) == 0 {
		return
	}
	translationsMu.Lock()
	defer translationsMu.Unlock()
	translations[key] = append(translations[key], msgs)
}

// TranslationsFor returns the messages of the model registered under key:
// instance.DefineTranslations() (when implemented) followed by every set
// registered with AddTranslations, in registration order — apply them in
// order so the later sets override. Keys are returned as declared (possibly
// model-relative).
func TranslationsFor(key string, instance any) []map[string]map[string]string {
	var out []map[string]map[string]string
	if ht, ok := instance.(HasTranslations); ok {
		if m := ht.DefineTranslations(); len(m) > 0 {
			out = append(out, m)
		}
	}
	translationsMu.RLock()
	out = append(out, translations[key]...)
	translationsMu.RUnlock()
	return out
}
