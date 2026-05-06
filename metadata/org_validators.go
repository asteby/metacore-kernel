package metadata

import (
	"context"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// OrgConfigResolver resolves the value of a `$org.<key>` reference declared
// inside ColumnDef.Validation.Custom or FieldDef.Validation. Apps wire it
// from their org-config store: the kernel never knows what an "RFC" or
// "NIT" is — it only knows how to swap a token for the validator identifier
// the org actually wants applied.
//
// Returning ("", false) means the key is not configured for the current org;
// the metadata service then leaves the literal `$org.<key>` reference in the
// payload so the SDK can decide whether to fall back to a built-in default
// or surface the missing config to the operator.
//
// The resolver is per-request because org context lives on the
// context.Context the caller passes through GetTable / GetModal — apps
// extract `orgID := authctx.OrgFromContext(ctx)` inside the closure they
// hand to WithOrgConfigResolver.
type OrgConfigResolver func(ctx context.Context, key string) (string, bool)

// orgRefPrefix is the discriminator the metadata service looks for. Any
// Validation value that starts with this is treated as a reference to be
// resolved through the OrgConfigResolver. Plain literals (e.g. "email",
// "rfc.tax_id") pass through untouched so the legacy authoring style keeps
// working.
const orgRefPrefix = "$org."

// WithOrgConfigResolver registers fn as the per-request resolver used by
// ResolveOrgValidators. Returns the receiver so calls chain fluently. Pass
// nil to clear a previously installed resolver.
func (s *Service) WithOrgConfigResolver(fn OrgConfigResolver) *Service {
	s.mu.Lock()
	s.orgResolver = fn
	s.mu.Unlock()
	if fn != nil {
		// Wire the table+modal transformers exactly once. Subsequent
		// WithOrgConfigResolver calls only swap the resolver pointer; the
		// transformers read it from the service via closures so the latest
		// resolver always wins.
		s.mu.Lock()
		alreadyWired := s.orgValidatorsWired
		s.orgValidatorsWired = true
		s.mu.Unlock()
		if !alreadyWired {
			s.WithTableTransformer(s.tableOrgValidatorTransformer())
			s.WithModalTransformer(s.modalOrgValidatorTransformer())
		}
	}
	return s
}

// orgResolverSnapshot returns the currently registered resolver under the
// service lock so callers don't race with WithOrgConfigResolver swaps.
func (s *Service) orgResolverSnapshot() OrgConfigResolver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.orgResolver
}

func (s *Service) tableOrgValidatorTransformer() TableTransformer {
	return func(ctx context.Context, _ string, meta *modelbase.TableMetadata) error {
		resolver := s.orgResolverSnapshot()
		if resolver == nil || meta == nil {
			return nil
		}
		for i := range meta.Columns {
			if meta.Columns[i].Validation == nil {
				continue
			}
			meta.Columns[i].Validation.Custom = resolveOrgRef(ctx, meta.Columns[i].Validation.Custom, resolver)
		}
		return nil
	}
}

func (s *Service) modalOrgValidatorTransformer() ModalTransformer {
	return func(ctx context.Context, _ string, meta *modelbase.ModalMetadata) error {
		resolver := s.orgResolverSnapshot()
		if resolver == nil || meta == nil {
			return nil
		}
		for i := range meta.Fields {
			meta.Fields[i].Validation = resolveOrgRef(ctx, meta.Fields[i].Validation, resolver)
		}
		return nil
	}
}

// resolveOrgRef resolves a single Validation token. Plain literals pass
// through; `$org.<key>` references are swapped for the resolver's value or,
// when the key is not configured, left in place so the SDK can decide what
// to do (typical fallback: skip the validator, log a warning).
func resolveOrgRef(ctx context.Context, value string, resolver OrgConfigResolver) string {
	if value == "" || !strings.HasPrefix(value, orgRefPrefix) {
		return value
	}
	key := strings.TrimPrefix(value, orgRefPrefix)
	if key == "" {
		return value
	}
	if resolved, ok := resolver(ctx, key); ok && resolved != "" {
		return resolved
	}
	return value
}
