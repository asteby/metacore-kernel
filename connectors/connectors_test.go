package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// mapStore is an in-memory Store keyed by (org, connector).
type mapStore struct {
	creds map[uuid.UUID]map[string]map[string]string
	err   error
}

func (s *mapStore) Get(_ context.Context, orgID uuid.UUID, key string) (map[string]string, bool, error) {
	if s.err != nil {
		return nil, false, s.err
	}
	byConn, ok := s.creds[orgID]
	if !ok {
		return nil, false, nil
	}
	c, ok := byConn[key]
	return c, ok, nil
}

func newStore(org uuid.UUID) *mapStore {
	return &mapStore{creds: map[uuid.UUID]map[string]map[string]string{
		org: {
			"github": {"access_token": "ghp_x", "webhook_secret": "shh"},
		},
	}}
}

func TestResolverGet(t *testing.T) {
	org := uuid.New()
	r := NewResolver(newStore(org))
	creds, err := r.Get(context.Background(), org, "github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if creds["access_token"] != "ghp_x" {
		t.Fatalf("want ghp_x, got %q", creds["access_token"])
	}
}

func TestResolverGetUnknownConnector(t *testing.T) {
	org := uuid.New()
	r := NewResolver(newStore(org))
	if _, err := r.Get(context.Background(), org, "slack"); !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("want ErrConnectorNotFound, got %v", err)
	}
}

func TestResolverNilStoreIsFeatureOff(t *testing.T) {
	// Both a nil *Resolver and a Resolver over a nil Store are the off state.
	var nilResolver *Resolver
	if _, err := nilResolver.Get(context.Background(), uuid.New(), "github"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil resolver: want ErrNoStore, got %v", err)
	}
	r := NewResolver(nil)
	if _, err := r.Get(context.Background(), uuid.New(), "github"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store: want ErrNoStore, got %v", err)
	}
	if _, err := r.Secret(context.Background(), uuid.New(), "github.webhook_secret"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store secret: want ErrNoStore, got %v", err)
	}
}

func TestResolverSecret(t *testing.T) {
	org := uuid.New()
	r := NewResolver(newStore(org))
	secret, err := r.Secret(context.Background(), org, "github.webhook_secret")
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if secret != "shh" {
		t.Fatalf("want shh, got %q", secret)
	}
}

func TestResolverSecretInvalidRef(t *testing.T) {
	org := uuid.New()
	r := NewResolver(newStore(org))
	for _, ref := range []string{"github", "", ".webhook_secret", "github."} {
		if _, err := r.Secret(context.Background(), org, ref); !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("ref %q: want ErrInvalidRef, got %v", ref, err)
		}
	}
}

func TestResolverSecretMissingCredential(t *testing.T) {
	org := uuid.New()
	r := NewResolver(newStore(org))
	if _, err := r.Secret(context.Background(), org, "github.nope"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("want ErrCredentialNotFound, got %v", err)
	}
}
