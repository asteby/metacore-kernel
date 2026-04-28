package metadata

import (
	"context"
	"strings"

	"github.com/asteby/metacore-kernel/i18n"
	"github.com/asteby/metacore-kernel/modelbase"
)

// DefaultI18nKeyPrefix is the prefix the LocalizedTransformers look for
// when deciding whether a string field is an i18n key. Apps that adopt a
// different convention can pass their own prefix to the constructors.
const DefaultI18nKeyPrefix = "models."

// NewLocalizedTableTransformer returns a TableTransformer that walks a
// TableMetadata and replaces any string field starting with `prefix`
// with `translator.Translate(ctx, key)`. Pass an empty prefix ("") to
// translate every string field unconditionally.
//
// The transformer modifies meta in-place and never returns an error;
// translators that fail to find a key should return the key itself, so
// the worst case is a visible "models.foo.bar" in the UI.
func NewLocalizedTableTransformer(translator i18n.Translator, prefix string) TableTransformer {
	if translator == nil {
		return nil
	}
	if prefix == "" {
		prefix = DefaultI18nKeyPrefix
	}
	t := translatorWalker{tr: translator, prefix: prefix}
	return func(ctx context.Context, _ string, meta *modelbase.TableMetadata) error {
		if meta == nil {
			return nil
		}
		t.translateString(ctx, &meta.Title)
		t.translateString(ctx, &meta.SearchPlaceholder)
		for i := range meta.Columns {
			t.translateColumn(ctx, &meta.Columns[i])
		}
		for i := range meta.Filters {
			t.translateFilter(ctx, &meta.Filters[i])
		}
		for i := range meta.Actions {
			t.translateAction(ctx, &meta.Actions[i])
		}
		return nil
	}
}

// NewLocalizedModalTransformer is the ModalMetadata counterpart of
// NewLocalizedTableTransformer.
func NewLocalizedModalTransformer(translator i18n.Translator, prefix string) ModalTransformer {
	if translator == nil {
		return nil
	}
	if prefix == "" {
		prefix = DefaultI18nKeyPrefix
	}
	t := translatorWalker{tr: translator, prefix: prefix}
	return func(ctx context.Context, _ string, meta *modelbase.ModalMetadata) error {
		if meta == nil {
			return nil
		}
		t.translateString(ctx, &meta.Title)
		t.translateString(ctx, &meta.CreateTitle)
		t.translateString(ctx, &meta.EditTitle)
		t.translateString(ctx, &meta.DeleteTitle)
		for i := range meta.Fields {
			t.translateField(ctx, &meta.Fields[i])
		}
		for i := range meta.CustomActions {
			t.translateAction(ctx, &meta.CustomActions[i])
		}
		for k, v := range meta.Messages {
			meta.Messages[k] = t.maybeTranslate(ctx, v)
		}
		return nil
	}
}

// translatorWalker carries the translator + prefix so per-field helpers
// stay terse. Not exported because the constructors are the API.
type translatorWalker struct {
	tr     i18n.Translator
	prefix string
}

func (t translatorWalker) maybeTranslate(ctx context.Context, value string) string {
	if value == "" || !strings.HasPrefix(value, t.prefix) {
		return value
	}
	return t.tr.Translate(ctx, value)
}

func (t translatorWalker) translateString(ctx context.Context, ptr *string) {
	if ptr == nil {
		return
	}
	*ptr = t.maybeTranslate(ctx, *ptr)
}

func (t translatorWalker) translateColumn(ctx context.Context, col *modelbase.ColumnDef) {
	t.translateString(ctx, &col.Label)
	t.translateString(ctx, &col.Tooltip)
	t.translateString(ctx, &col.Description)
	for i := range col.Options {
		t.translateString(ctx, &col.Options[i].Label)
	}
}

func (t translatorWalker) translateFilter(ctx context.Context, f *modelbase.FilterDef) {
	t.translateString(ctx, &f.Label)
	for i := range f.Options {
		t.translateString(ctx, &f.Options[i].Label)
	}
}

func (t translatorWalker) translateField(ctx context.Context, f *modelbase.FieldDef) {
	t.translateString(ctx, &f.Label)
	t.translateString(ctx, &f.Placeholder)
	for i := range f.Options {
		t.translateString(ctx, &f.Options[i].Label)
	}
}

func (t translatorWalker) translateAction(ctx context.Context, a *modelbase.ActionDef) {
	t.translateString(ctx, &a.Label)
	t.translateString(ctx, &a.ConfirmMessage)
	for i := range a.Fields {
		t.translateField(ctx, &a.Fields[i])
	}
}
