package dataload

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Registro de perfiles versionados por organización.
//
// Un perfil publicado es inmutable: cada versión es un registro que no cambia. Hay dos
// formas de publicarlo:
//
//   - completo: trae su Profile (y, opcional, su documento de extracción);
//   - derivado: apunta a otra versión (Base) y trae solo los Overrides de la
//     organización. La base se fija a una versión exacta, así lo resuelto no
//     cambia cuando alguien publica una versión nueva de la base. Base.Org vacío
//     es el catálogo base compartido; Base.Org igual a la organización, una
//     versión propia anterior.
//
// El catálogo base compartido vive bajo la organización "" ; cada organización
// publica lo suyo bajo su id. Una organización solo lee lo suyo y el catálogo base.
// ProfileStore es solo persistencia (la implementan ops/addons); la lógica de
// validar, resolver y huellear vive en Registry.

// ProfileKey identifica una versión publicada.
type ProfileKey struct {
	Org     string `json:"org"` // "" = catálogo base compartido
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (k ProfileKey) String() string {
	org := k.Org
	if org == "" {
		org = "base"
	}
	return org + "/" + k.Name + "@" + k.Version
}

// StoredProfile es una versión publicada.
type StoredProfile struct {
	ProfileKey
	// Profile: perfil completo. Excluyente con Base.
	Profile *Profile `json:"profile,omitempty"`
	// Base + Overrides: perfil derivado. Base.Version es obligatoria.
	Base      *ProfileKey `json:"base,omitempty"`
	Overrides *Overrides  `json:"overrides,omitempty"`
	// Extract: documento de extracción de esta versión; en un derivado, si falta
	// se hereda el de la base.
	Extract *ExtractDoc `json:"extract,omitempty"`

	Note      string    `json:"note,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// Fingerprint es la huella del contenido publicado (la calcula Registry).
	Fingerprint string `json:"fingerprint"`
}

// Errores del registro.
var (
	ErrProfileNotFound = newErr(CodeProfileNotFound)
	// ErrProfileVersionExists: la versión ya existe con otro contenido; las versiones no se reescriben.
	ErrProfileVersionExists = newErr(CodeProfileVersionExists)
)

// ProfileStore es la persistencia de versiones de perfiles.
type ProfileStore interface {
	// Put inserta una versión. Si (Org, Name, Version) ya existe con la misma
	// Fingerprint no hace nada; con otra, devuelve ErrProfileVersionExists.
	Put(ctx context.Context, sp StoredProfile) error
	// Get devuelve una versión exacta; con version "" devuelve la última publicada.
	Get(ctx context.Context, org, name, version string) (StoredProfile, bool, error)
	// List devuelve las versiones de la organización (y nombre, si se indica) de la más vieja a la más nueva.
	List(ctx context.Context, org, name string) ([]StoredProfile, error)
}

// Registry publica y resuelve perfiles sobre un ProfileStore.
type Registry struct {
	Store ProfileStore
	Now   func() time.Time
}

const maxProfileDepth = 8

func (g *Registry) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

// Fingerprint calcula la huella del contenido de una versión (sin metadatos de autor ni fecha).
func (sp StoredProfile) contentHash() string {
	return canonicalHash(struct {
		K ProfileKey  `json:"k"`
		P *Profile    `json:"p,omitempty"`
		B *ProfileKey `json:"b,omitempty"`
		O *Overrides  `json:"o,omitempty"`
		E *ExtractDoc `json:"e,omitempty"`
	}{sp.ProfileKey, sp.Profile, sp.Base, sp.Overrides, sp.Extract})
}

// Publish valida (incluida la resolución completa con su base) y guarda una
// versión nueva. Publicar de nuevo exactamente lo mismo es no-op.
func (g *Registry) Publish(ctx context.Context, sp StoredProfile) (StoredProfile, error) {
	var errs []Message
	bad := func(code Code, kv ...any) { errs = append(errs, msg(code, kv...)) }
	if sp.Name == "" || sp.Version == "" {
		bad(CodeRegistryMissingName)
	}
	switch {
	case sp.Profile != nil && sp.Base != nil:
		bad(CodeRegistryProfileAndBase)
	case sp.Profile == nil && sp.Base == nil:
		bad(CodeRegistryNoProfileOrBase)
	}
	if sp.Profile != nil {
		if sp.Profile.Name != sp.Name || sp.Profile.Version != sp.Version {
			bad(CodeRegistryNameMismatch, "profile", sp.Profile.Name+"@"+sp.Profile.Version, "published", sp.Name+"@"+sp.Version)
		}
		if sp.Overrides != nil {
			bad(CodeRegistryFullWithOverrides)
		}
	}
	if sp.Base != nil {
		if sp.Base.Name == "" || sp.Base.Version == "" {
			bad(CodeRegistryBaseIncomplete)
		}
		if sp.Base.Org != "" && sp.Base.Org != sp.Org {
			bad(CodeRegistryBaseForeign, "org", sp.Base.Org)
		}
		if *sp.Base == sp.ProfileKey {
			bad(CodeRegistryBaseSelf)
		}
	}
	if sp.Overrides != nil && sp.Overrides.Org != "" && sp.Overrides.Org != sp.Org {
		bad(CodeRegistryOverridesForeign, "got", sp.Overrides.Org, "want", sp.Org)
	}
	if err := detailsErr(errs, CodeRegistryInvalid, "key", sp.ProfileKey.String()); err != nil {
		return StoredProfile{}, err
	}
	sp.Fingerprint = sp.contentHash()
	if prev, ok, err := g.Store.Get(ctx, sp.Org, sp.Name, sp.Version); err != nil {
		return StoredProfile{}, err
	} else if ok {
		if prev.Fingerprint == sp.Fingerprint {
			return prev, nil
		}
		return StoredProfile{}, newErr(CodeProfileVersionExists, "key", sp.ProfileKey.String())
	}
	// Resolver antes de guardar: no se publica nada que no se pueda usar.
	if _, err := g.resolveRecord(ctx, sp, map[ProfileKey]bool{}, 0); err != nil {
		return StoredProfile{}, err
	}
	sp.CreatedAt = g.now()
	if err := g.Store.Put(ctx, sp); err != nil {
		return StoredProfile{}, err
	}
	return sp, nil
}

// ResolvedProfile es el resultado de resolver una versión: lo que se pasa a una corrida.
type ResolvedProfile struct {
	// Key es la versión pedida (ya con la organización de donde salió).
	Key ProfileKey `json:"key"`
	// Profile ya trae los overrides aplicados y está validado.
	Profile Profile `json:"profile"`
	// Extract es el documento de extracción vigente; nil si no hay.
	Extract *Extractor `json:"-"`
	// Chain va de la versión pedida a la base raíz.
	Chain []ProfileKey `json:"chain"`
	// OverridesHash resume los overrides aplicados en la cadena.
	OverridesHash string `json:"overridesHash,omitempty"`
	// Fingerprint es la huella de lo resuelto (perfil + extracción): dos
	// resoluciones con la misma huella producen el mismo plan sobre la misma entrada.
	Fingerprint string `json:"fingerprint"`
}

// Resolve devuelve la versión pedida (version "" = la última) de la organización,
// con su base y overrides aplicados. Si la organización no tiene ese nombre,
// usa el catálogo base compartido.
func (g *Registry) Resolve(ctx context.Context, org, name, version string) (ResolvedProfile, error) {
	sp, ok, err := g.Store.Get(ctx, org, name, version)
	if err != nil {
		return ResolvedProfile{}, err
	}
	if !ok && org != "" {
		sp, ok, err = g.Store.Get(ctx, "", name, version)
		if err != nil {
			return ResolvedProfile{}, err
		}
	}
	if !ok {
		return ResolvedProfile{}, newErr(CodeProfileNotFound, "key", ProfileKey{Org: org, Name: name, Version: version}.String())
	}
	return g.resolveRecord(ctx, sp, map[ProfileKey]bool{}, 0)
}

// resolveRecord resuelve sp siguiendo su cadena de bases (cada una a su versión exacta).
func (g *Registry) resolveRecord(ctx context.Context, sp StoredProfile, seen map[ProfileKey]bool, depth int) (ResolvedProfile, error) {
	if depth > maxProfileDepth {
		return ResolvedProfile{}, newErr(CodeRegistryChainTooLong, "key", sp.ProfileKey.String())
	}
	if seen[sp.ProfileKey] {
		return ResolvedProfile{}, newErr(CodeRegistryCycle, "key", sp.ProfileKey.String())
	}
	seen[sp.ProfileKey] = true

	out := ResolvedProfile{Key: sp.ProfileKey, Chain: []ProfileKey{sp.ProfileKey}}
	var ex *ExtractDoc
	if sp.Profile != nil {
		p, err := sp.Profile.Resolve(nil)
		if err != nil {
			return ResolvedProfile{}, err
		}
		out.Profile = p
	} else {
		base, ok, err := g.Store.Get(ctx, sp.Base.Org, sp.Base.Name, sp.Base.Version)
		if err != nil {
			return ResolvedProfile{}, err
		}
		if !ok {
			return ResolvedProfile{}, newErr(CodeProfileBaseNotFound, "base", sp.Base.String(), "key", sp.ProfileKey.String())
		}
		parent, err := g.resolveRecord(ctx, base, seen, depth+1)
		if err != nil {
			return ResolvedProfile{}, err
		}
		p, err := parent.Profile.Resolve(sp.Overrides)
		if err != nil {
			return ResolvedProfile{}, wrapErr(err, CodeRegistryAt, "key", sp.ProfileKey.String())
		}
		out.Profile, out.Chain, out.OverridesHash = p, append(out.Chain, parent.Chain...), parent.OverridesHash
		if parent.Extract != nil {
			ex = &parent.Extract.Doc
		}
	}
	if sp.Overrides != nil {
		out.OverridesHash = canonicalHash([]string{out.OverridesHash, sp.Overrides.Hash()})
	}
	if sp.Extract != nil {
		ex = sp.Extract
	}
	if ex != nil {
		e, err := NewExtractor(*ex)
		if err != nil {
			return ResolvedProfile{}, err
		}
		if err := e.CheckProfile(out.Profile); err != nil {
			return ResolvedProfile{}, wrapErr(err, CodeRegistryAt, "key", sp.ProfileKey.String())
		}
		out.Extract = e
	}
	out.Fingerprint = fingerprint(out.Profile.Hash(), out.Extract)
	return out, nil
}

// Request arma la solicitud de corrida con lo resuelto. La huella del registro
// queda en la corrida para poder reconstruir con qué versión se hizo.
func (r ResolvedProfile) Request(org, actor string, src Source) RunRequest {
	return RunRequest{Org: org, Actor: actor, Profile: r.Profile, Extract: r.Extract, Source: src, Registry: &RegistryRef{Key: r.Key, Fingerprint: r.Fingerprint}}
}

// RegistryRef liga una corrida con la versión del registro de la que salió su perfil.
type RegistryRef struct {
	Key         ProfileKey `json:"key"`
	Fingerprint string     `json:"fingerprint"`
}

// MemoryProfileStore es un ProfileStore en memoria.
type MemoryProfileStore struct {
	mu   sync.Mutex
	recs []StoredProfile // en orden de publicación
}

func NewMemoryProfileStore() *MemoryProfileStore { return &MemoryProfileStore{} }

func (s *MemoryProfileStore) Put(_ context.Context, sp StoredProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.ProfileKey == sp.ProfileKey {
			if r.Fingerprint == sp.Fingerprint {
				return nil
			}
			return newErr(CodeProfileVersionExists, "key", sp.ProfileKey.String())
		}
	}
	s.recs = append(s.recs, sp)
	return nil
}

func (s *MemoryProfileStore) Get(_ context.Context, org, name, version string) (StoredProfile, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.recs) - 1; i >= 0; i-- {
		r := s.recs[i]
		if r.Org == org && r.Name == name && (version == "" || r.Version == version) {
			return r, true, nil
		}
	}
	return StoredProfile{}, false, nil
}

func (s *MemoryProfileStore) List(_ context.Context, org, name string) ([]StoredProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StoredProfile
	for _, r := range s.recs {
		if r.Org == org && (name == "" || r.Name == name) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Versions lista los nombres y versiones visibles para una organización (las
// suyas y las del catálogo base), útil para pantallas de selección.
func (g *Registry) Versions(ctx context.Context, org string) ([]ProfileKey, error) {
	var keys []ProfileKey
	for _, o := range []string{"", org} {
		if o == "" && org == "" && len(keys) > 0 {
			break
		}
		list, err := g.Store.List(ctx, o, "")
		if err != nil {
			return nil, err
		}
		for _, sp := range list {
			keys = append(keys, sp.ProfileKey)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].Name != keys[j].Name {
			return keys[i].Name < keys[j].Name
		}
		return keys[i].Org < keys[j].Org
	})
	return keys, nil
}
