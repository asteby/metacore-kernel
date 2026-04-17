package modelbase_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

type fakeModel struct{ n int }

func (f *fakeModel) TableName() string                      { return "fakes" }
func (f *fakeModel) DefineTable() modelbase.TableMetadata   { return modelbase.TableMetadata{Title: "fakes"} }
func (f *fakeModel) DefineModal() modelbase.ModalMetadata   { return modelbase.ModalMetadata{Title: "fake"} }

func TestRegistryReturnsFreshInstances(t *testing.T) {
	counter := 0
	modelbase.Register("fakes", func() modelbase.ModelDefiner {
		counter++
		return &fakeModel{n: counter}
	})

	a, ok := modelbase.Get("fakes")
	if !ok {
		t.Fatal("Get(fakes) not found")
	}
	b, ok := modelbase.Get("fakes")
	if !ok {
		t.Fatal("Get(fakes) not found (2)")
	}
	if a == b {
		t.Fatal("factory must return fresh instances")
	}
	if a.TableName() != "fakes" {
		t.Fatalf("TableName: got %q", a.TableName())
	}

	all := modelbase.All()
	if _, ok := all["fakes"]; !ok {
		t.Fatal("All() missing fakes key")
	}
}

func TestRegistryIgnoresEmptyKeyOrNilFactory(t *testing.T) {
	modelbase.Register("", func() modelbase.ModelDefiner { return &fakeModel{} })
	modelbase.Register("nilfactory", nil)
	if _, ok := modelbase.Get(""); ok {
		t.Fatal("empty key must not be registered")
	}
	if _, ok := modelbase.Get("nilfactory"); ok {
		t.Fatal("nil factory must not be registered")
	}
}
