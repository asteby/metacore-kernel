package batch

import (
	"context"
	"strings"

	"github.com/asteby/metacore-kernel/dataload"
)

// SourceFor exposes a batch File as a dataload.Source so it can feed an
// existing Profile without a parser of its own. format is "csv", "json"
// (array of objects, or {"data":[...]}) or "bundle:<dotted.path>" (an object
// whose rows live at that path). An unknown format yields a Source whose Rows
// fails with dataload.batch.format.
func SourceFor(f File, format string) dataload.Source {
	name := "batch:" + f.Name
	switch kind, path, _ := strings.Cut(format, ":"); strings.ToLower(kind) {
	case "csv":
		return dataload.NewCSVSource(name, f.Body)
	case "json":
		return dataload.NewJSONSource(name, f.Body)
	case "bundle":
		return dataload.NewBundleSource(name, f.Body, path)
	}
	return badFormat{name: name, format: format}
}

type badFormat struct{ name, format string }

func (b badFormat) Name() string { return b.name }
func (b badFormat) Rows(context.Context) ([]map[string]any, error) {
	return nil, dataload.NewError(dataload.CodeBatchFormat, "format", b.format)
}
