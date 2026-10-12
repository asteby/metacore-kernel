package agent

import "strings"

// ResolveModelKey maps whatever the LLM called a model (sales_order, invoice,
// "customers.model.customer") onto a registered key, using only the host's
// ModelCatalog. It never returns a dotted i18n key: those blow up Postgres as
// cross-database references, so an unresolvable dotted name returns "".
func ResolveModelKey(cat ModelCatalog, modelKey string) string {
	key := strings.TrimSpace(modelKey)
	if key == "" || cat == nil {
		return key
	}
	if resolved := lookupRegisteredModel(cat, key); resolved != "" {
		return preferTableKey(cat, resolved)
	}
	if src, ok := cat.AliasSource(key); ok && src != "" {
		if resolved := lookupRegisteredModel(cat, src); resolved != "" {
			return preferTableKey(cat, resolved)
		}
	}

	// Exact label match: the LLM copied "inventory.model.stock" from a listing.
	if strings.Contains(key, ".") {
		if resolved := lookupModelByLabel(cat, key); resolved != "" {
			return preferTableKey(cat, resolved)
		}
		// i18n / dotted labels: "customers.model.customer" -> "customer", "customers"
		segs := strings.Split(key, ".")
		for i := len(segs) - 1; i >= 0; i-- {
			seg := strings.TrimSpace(segs[i])
			switch seg {
			case "", "model", "field", "nav", "title":
				continue
			}
			if resolved := lookupRegisteredModel(cat, seg); resolved != "" {
				return preferTableKey(cat, resolved)
			}
			if resolved := resolveModelKeyCandidates(cat, seg); resolved != "" {
				return preferTableKey(cat, resolved)
			}
		}
	}

	if resolved := resolveModelKeyCandidates(cat, key); resolved != "" {
		return preferTableKey(cat, resolved)
	}
	if strings.Contains(key, ".") {
		return ""
	}
	return key
}

func preferTableKey(cat ModelCatalog, key string) string {
	if m, ok := cat.Lookup(key); ok && m.Table != "" {
		return m.Table
	}
	return key
}

func lookupRegisteredModel(cat ModelCatalog, key string) string {
	if key == "" {
		return ""
	}
	if _, ok := cat.Lookup(key); ok {
		return key
	}
	return ""
}

func lookupModelByLabel(cat ModelCatalog, label string) string {
	want := strings.TrimSpace(label)
	if want == "" {
		return ""
	}
	for _, m := range cat.All() {
		if strings.EqualFold(strings.TrimSpace(m.Label), want) {
			switch {
			case m.Table != "":
				return m.Table
			case m.ModelKey != "":
				return m.ModelKey
			}
			return m.Key
		}
	}
	return ""
}

func resolveModelKeyCandidates(cat ModelCatalog, key string) string {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "" {
		return ""
	}
	for _, alt := range cat.Hints(lower) {
		if resolved := lookupRegisteredModel(cat, alt); resolved != "" {
			return resolved
		}
	}

	candidates := []string{key, lower}
	if !strings.HasSuffix(lower, "s") {
		candidates = append(candidates, lower+"s", lower+"es")
	} else {
		candidates = append(candidates, strings.TrimSuffix(lower, "s"), strings.TrimSuffix(lower, "es"))
	}
	candidates = append(candidates, snakeToPascal(lower))

	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if resolved := lookupRegisteredModel(cat, c); resolved != "" {
			return resolved
		}
	}

	want := normalizeModelToken(lower)
	for _, m := range cat.All() {
		if normalizeModelToken(m.Key) == want {
			return m.Key
		}
		if m.Table != "" && normalizeModelToken(m.Table) == want {
			return m.Table
		}
		if m.ModelKey != "" && normalizeModelToken(m.ModelKey) == want {
			if m.Table != "" {
				return m.Table
			}
			return m.ModelKey
		}
	}
	return ""
}

func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

func normalizeModelToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	// collapse singular/plural so sales_order matches sales_orders
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		s = s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "ses") && len(s) > 3:
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "s") && len(s) > 2:
		s = s[:len(s)-1]
	}
	return s
}

// ModelLabel is the user-facing name of a model: its translated title/label,
// or the humanized key as a last resort ("datos" for an empty key).
func ModelLabel(cat ModelCatalog, modelKey string) string {
	if modelKey == "" {
		return "datos"
	}
	key := ResolveModelKey(cat, modelKey)
	if cat == nil {
		return humanizeSnake(key)
	}
	translate := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return ""
		}
		if t := cat.Translate(raw); t != "" && t != raw {
			return t
		}
		// Already a human label (not an i18n key).
		if !strings.Contains(raw, ".") {
			return raw
		}
		return ""
	}
	if m, ok := cat.Lookup(key); ok {
		if t := translate(m.Title); t != "" {
			return t
		}
		if t := translate(m.Label); t != "" {
			return t
		}
	}
	return humanizeSnake(key)
}

func humanizeSnake(key string) string {
	key = strings.ReplaceAll(key, "_", " ")
	key = strings.ReplaceAll(key, "-", " ")
	parts := strings.Fields(key)
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// ModelIcon is a lucide slug for the model: the host's navigation icon when it
// has one, otherwise "database".
func ModelIcon(cat ModelCatalog, modelKey string) string {
	if modelKey == "" || cat == nil {
		return "database"
	}
	if icon := cat.NavIcon(ResolveModelKey(cat, modelKey)); icon != "" {
		return lucideKebab(icon)
	}
	return "database"
}

func lucideKebab(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "database"
	}
	if strings.Contains(name, "-") && name == strings.ToLower(name) {
		return name
	}
	var b strings.Builder
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r + ('a' - 'A'))
		case r == '_' || r == ' ':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.ReplaceAll(strings.ToLower(b.String()), "--", "-")
	return strings.Trim(out, "-")
}

// CatalogRegistry adapts a ModelCatalog to the ModelRegistry the embed
// builder consumes. FieldLabel and EnumValueLabel fall back to the raw value
// unless the host supplies Labels.
type CatalogRegistry struct {
	Catalog ModelCatalog
	// Labels optionally localizes column and enum labels.
	Labels interface {
		FieldLabel(model, key, raw string) string
		EnumValueLabel(model, field, value string) string
	}
}

// ResolveModelKey implements ModelRegistry.
func (r CatalogRegistry) ResolveModelKey(name string) string { return ResolveModelKey(r.Catalog, name) }

// ModelLabel implements ModelRegistry.
func (r CatalogRegistry) ModelLabel(modelKey string) string { return ModelLabel(r.Catalog, modelKey) }

// FieldLabel implements ModelRegistry.
func (r CatalogRegistry) FieldLabel(model, key, raw string) string {
	if r.Labels != nil {
		return r.Labels.FieldLabel(model, key, raw)
	}
	return raw
}

// EnumValueLabel implements ModelRegistry.
func (r CatalogRegistry) EnumValueLabel(model, field, value string) string {
	if r.Labels != nil {
		return r.Labels.EnumValueLabel(model, field, value)
	}
	return value
}
