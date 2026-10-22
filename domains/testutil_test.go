package domains

import (
	"context"
	"sync"
)

// fakeResolver answers from a static map keyed "TYPE|name".
type fakeResolver struct {
	name string
	mu   sync.Mutex
	data map[string][]string
	errs map[string]error
	hits int
}

func newFake(name string) *fakeResolver {
	return &fakeResolver{name: name, data: map[string][]string{}, errs: map[string]error{}}
}

func (f *fakeResolver) Name() string { return f.name }
func (f *fakeResolver) set(t RecordType, name string, vals ...string) *fakeResolver {
	f.data[string(t)+"|"+name] = vals
	return f
}
func (f *fakeResolver) fail(t RecordType, name string, err error) *fakeResolver {
	f.errs[string(t)+"|"+name] = err
	return f
}
func (f *fakeResolver) Lookup(_ context.Context, name string, t RecordType) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	k := string(t) + "|" + name
	if err, ok := f.errs[k]; ok {
		return nil, err
	}
	if v, ok := f.data[k]; ok && len(v) > 0 {
		return v, nil
	}
	return nil, ErrNoRecords
}
