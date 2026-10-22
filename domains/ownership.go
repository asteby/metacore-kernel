package domains

import (
	"context"
	"errors"
	"strings"
)

// ContainsToken reports whether any TXT value equals token (surrounding quotes
// and whitespace ignored). Exact match, not substring: a longer value that
// merely contains the token does not prove ownership. Same rule as the
// storefront custom-domain flow.
func ContainsToken(records []string, token string) bool {
	if token == "" {
		return false
	}
	for _, r := range records {
		if strings.TrimSpace(strings.Trim(strings.TrimSpace(r), `"`)) == token {
			return true
		}
	}
	return false
}

// OwnershipResult is the outcome of the TXT ownership proof.
type OwnershipResult struct {
	// Verified is true only when the token was found.
	Verified bool `json:"verified"`
	// Status: ok | missing | wrong_value | unknown.
	Status RecordStatus `json:"status"`
	// Record is the TXT the tenant must publish.
	Record     Record      `json:"record"`
	Observed   []string    `json:"observed,omitempty"`
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
}

// CheckOwnership looks up _asteby-verify.<host> and checks for token.
//
//	ok           token present
//	missing      the name has no TXT at all
//	wrong_value  TXT exists but none carries the token
//	unknown      the lookup failed (timeout, SERVFAIL): retry, do not tell the
//	             tenant their record is absent
//
// Only Verified==true may flip an OrgDomain to StatusVerified.
func CheckOwnership(ctx context.Context, r DNSResolver, host, token string) OwnershipResult {
	rec := Record{Type: TypeTXT, Name: VerifyRecordName(host), Value: token, Purpose: "ownership"}
	out := OwnershipResult{Record: rec}
	if token == "" || host == "" {
		out.Status = RecordMissing
		d := diag(DiagTokenMissingInput, SeverityError, nil, "domain has no verification token")
		out.Diagnostic = &d
		return out
	}
	vals, err := r.Lookup(ctx, rec.Name, TypeTXT)
	switch {
	case err != nil && !errors.Is(err, ErrNoRecords):
		out.Status = RecordUnknown
		d := diag(DiagLookupFailed, SeverityWarn, map[string]any{"resolver": r.Name()}, err.Error())
		out.Diagnostic = &d
	case err != nil || len(vals) == 0:
		out.Status = RecordMissing
		d := diag(DiagTokenNotPublished, SeverityError, map[string]any{"name": rec.Name}, "")
		out.Diagnostic = &d
	case ContainsToken(vals, token):
		out.Status, out.Verified, out.Observed = RecordOK, true, vals
	default:
		out.Status, out.Observed = RecordWrongValue, vals
		d := diag(DiagTokenMismatch, SeverityError, map[string]any{"name": rec.Name}, "")
		out.Diagnostic = &d
	}
	return out
}
