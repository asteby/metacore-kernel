package httpx

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// fakeCtx is an in-memory ContextLookup for tests.
type fakeCtx map[string]any

func (f fakeCtx) Locals(key string) any { return f[key] }

func TestExtractOrgID(t *testing.T) {
	orgID := uuid.New()

	t.Run("happy path", func(t *testing.T) {
		got, err := ExtractOrgID(fakeCtx{LocalOrganizationID: orgID})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != orgID {
			t.Fatalf("got %s, want %s", got, orgID)
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := ExtractOrgID(fakeCtx{})
		if !errors.Is(err, ErrOrgMissing) {
			t.Fatalf("got %v, want ErrOrgMissing", err)
		}
	})

	t.Run("nil value", func(t *testing.T) {
		_, err := ExtractOrgID(fakeCtx{LocalOrganizationID: nil})
		if !errors.Is(err, ErrOrgMissing) {
			t.Fatalf("got %v, want ErrOrgMissing", err)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		_, err := ExtractOrgID(fakeCtx{LocalOrganizationID: "not-a-uuid"})
		if !errors.Is(err, ErrOrgInvalid) {
			t.Fatalf("got %v, want ErrOrgInvalid", err)
		}
	})

	t.Run("string uuid not auto-parsed", func(t *testing.T) {
		// We intentionally don't parse strings — middleware must store uuid.UUID.
		_, err := ExtractOrgID(fakeCtx{LocalOrganizationID: orgID.String()})
		if !errors.Is(err, ErrOrgInvalid) {
			t.Fatalf("got %v, want ErrOrgInvalid", err)
		}
	})
}

func TestExtractUserID(t *testing.T) {
	userID := uuid.New()

	t.Run("happy path", func(t *testing.T) {
		got, err := ExtractUserID(fakeCtx{LocalUserID: userID})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != userID {
			t.Fatalf("got %s, want %s", got, userID)
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := ExtractUserID(fakeCtx{})
		if !errors.Is(err, ErrUserMissing) {
			t.Fatalf("got %v, want ErrUserMissing", err)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		_, err := ExtractUserID(fakeCtx{LocalUserID: 42})
		if !errors.Is(err, ErrUserInvalid) {
			t.Fatalf("got %v, want ErrUserInvalid", err)
		}
	})
}

type inner struct {
	Name  string `json:"name"`
	Label string `json:"label_text"`
}

type outer struct {
	ID    uuid.UUID `json:"id"`
	Inner inner     `json:"inner"`
	Ptr   *inner    `json:"ptr"`
	Title string    `json:"title"`
}

type embedded struct {
	outer
	Extra string
}

func TestGetFieldValue(t *testing.T) {
	o := outer{
		ID:    uuid.New(),
		Inner: inner{Name: "alice", Label: "hello"},
		Ptr:   &inner{Name: "bob", Label: "world"},
		Title: "Mr",
	}
	v := reflect.ValueOf(o)

	t.Run("empty path", func(t *testing.T) {
		if got := GetFieldValue(v, ""); got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})

	t.Run("exact field", func(t *testing.T) {
		if got := GetFieldValue(v, "Title"); got != "Mr" {
			t.Fatalf("got %v, want Mr", got)
		}
	})

	t.Run("case-insensitive field", func(t *testing.T) {
		if got := GetFieldValue(v, "title"); got != "Mr" {
			t.Fatalf("got %v, want Mr", got)
		}
	})

	t.Run("nested struct", func(t *testing.T) {
		if got := GetFieldValue(v, "Inner.Name"); got != "alice" {
			t.Fatalf("got %v, want alice", got)
		}
	})

	t.Run("nested via json tag on root then field", func(t *testing.T) {
		if got := GetFieldValue(v, "inner.name"); got != "alice" {
			t.Fatalf("got %v, want alice", got)
		}
	})

	t.Run("nested via json tag resolving non-matching field name", func(t *testing.T) {
		// "label_text" only matches via json tag.
		if got := GetFieldValue(v, "inner.label_text"); got != "hello" {
			t.Fatalf("got %v, want hello", got)
		}
	})

	t.Run("pointer field dereferenced", func(t *testing.T) {
		if got := GetFieldValue(v, "Ptr.Name"); got != "bob" {
			t.Fatalf("got %v, want bob", got)
		}
	})

	t.Run("nil pointer yields nil", func(t *testing.T) {
		o2 := outer{Ptr: nil}
		if got := GetFieldValue(reflect.ValueOf(o2), "Ptr.Name"); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("missing field yields nil", func(t *testing.T) {
		if got := GetFieldValue(v, "Nope"); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("missing nested field yields nil", func(t *testing.T) {
		if got := GetFieldValue(v, "Inner.Nope"); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("promoted field via embedding", func(t *testing.T) {
		e := embedded{outer: o, Extra: "x"}
		if got := GetFieldValue(reflect.ValueOf(e), "Title"); got != "Mr" {
			t.Fatalf("got %v, want Mr (via promotion)", got)
		}
	})

	t.Run("non-struct early abort", func(t *testing.T) {
		if got := GetFieldValue(reflect.ValueOf(42), "Foo"); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("uuid field returned as uuid.UUID", func(t *testing.T) {
		got := GetFieldValue(v, "id")
		gotID, ok := got.(uuid.UUID)
		if !ok || gotID != o.ID {
			t.Fatalf("got %v (%T), want %v", got, got, o.ID)
		}
	})
}
