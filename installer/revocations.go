// revocations.go — consumption of the marketplace revocation list (C1).
//
// The hub publishes a signed list of revoked addon versions
// (GET /v1/marketplace/revocations, see hub/revocations.go). The installer:
//
//   - refuses to Install / Upgrade / Enable a revoked addon@version
//     (ErrRevoked), and
//   - exposes CheckRevocations, which a host calls from a periodic job to
//     Disable installations whose version was revoked after install.
//
// The list is verified with the SAME trust anchor as bundles
// (Installer.PublicKeys, see central_trust.go). Fail-safe: when the hub is
// unreachable the last valid cached list keeps being enforced, so a revocation
// already known is never forgotten.
package installer

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/asteby/metacore-kernel/hub"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ErrRevoked is returned (wrapped) when an addon version appears on the
// marketplace revocation list.
var ErrRevoked = errors.New("installer: addon version is revoked by the marketplace")

// RevocationSource yields the current verified revocation list. It is
// satisfied by *hub.RevocationChecker.
type RevocationSource interface {
	List(ctx context.Context) (*hub.RevocationList, error)
}

// WithRevocations wires a revocation source. With a source set, Install and
// Upgrade fail closed when no verified list can be obtained at all (cold
// start with the hub down); see WithRevocationsFailOpenWhenUnknown.
func (i *Installer) WithRevocations(src RevocationSource) *Installer {
	i.Revocations = src
	return i
}

// WithRevocationsFailOpenWhenUnknown relaxes Install/Upgrade to proceed (with
// a warning) when NO verified list has ever been obtained. A known revocation
// is still always enforced. Intended for air-gapped hosts that sideload.
func (i *Installer) WithRevocationsFailOpenWhenUnknown() *Installer {
	i.RevocationsFailOpenWhenUnknown = true
	return i
}

// UseHubRevocations builds a hub.RevocationChecker verified against this
// installer's trusted keys (read at verification time, so AppendTrustedPubKey
// rotations apply) and wires it in.
func (i *Installer) UseHubRevocations(c *hub.Client, opts ...hub.RevocationOption) *hub.RevocationChecker {
	rc := c.NewRevocationChecker(func() []ed25519.PublicKey { return i.PublicKeys }, opts...)
	i.WithRevocations(rc)
	return rc
}

// checkNotRevoked returns an ErrRevoked-wrapped error when addonKey@version is
// revoked. strict makes "no list obtainable" an error too (Install/Upgrade).
func (i *Installer) checkNotRevoked(ctx context.Context, addonKey, version string, strict bool) error {
	if i.Revocations == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	list, err := i.Revocations.List(ctx)
	if err != nil {
		if strict && !i.RevocationsFailOpenWhenUnknown {
			return fmt.Errorf("installer: cannot verify revocation status of %s@%s: %w", addonKey, version, err)
		}
		slog.Warn("installer.revocations_unavailable", "addon", addonKey, "version", version, "err", err.Error())
		return nil
	}
	if r, ok := list.Lookup(addonKey, version); ok {
		return fmt.Errorf("%w: %s@%s (%s)", ErrRevoked, addonKey, version, r.Reason)
	}
	return nil
}

// RevokedInstallation reports one installation CheckRevocations acted on.
type RevokedInstallation struct {
	OrganizationID uuid.UUID
	AddonKey       string
	Version        string
	Reason         string
	// Err is non-nil when the lifecycle Disable failed. The row was still
	// forced to status "disabled" (fail-closed).
	Err error
}

// CheckRevocations disables every enabled installation whose version is on the
// revocation list. Hosts call it from a periodic job (for example every few
// minutes). If the hub is down the last valid cached list is used; an error is
// returned only when no verified list is known at all (nothing can be
// disabled). Disable failures do not stop the sweep: the row is force-marked
// disabled and the failure is reported in the result and the returned error.
func (i *Installer) CheckRevocations(ctx context.Context) ([]RevokedInstallation, error) {
	if i.Revocations == nil {
		return nil, errors.New("installer: no revocation source configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	list, err := i.Revocations.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("installer: revocation list unavailable: %w", err)
	}
	var rows []Installation
	if err := i.DB.Where("status = ?", "enabled").Find(&rows).Error; err != nil {
		return nil, err
	}
	var out []RevokedInstallation
	var errs []error
	for _, row := range rows {
		rev, ok := list.Lookup(row.AddonKey, row.Version)
		if !ok {
			continue
		}
		res := RevokedInstallation{OrganizationID: row.OrganizationID, AddonKey: row.AddonKey, Version: row.Version, Reason: rev.Reason}
		if derr := i.Disable(row.OrganizationID, row.AddonKey); derr != nil {
			res.Err = derr
			errs = append(errs, fmt.Errorf("disable revoked %s@%s (org %s): %w", row.AddonKey, row.Version, row.OrganizationID, derr))
		}
		// Fail closed: whatever Disable did, a revoked addon must not stay enabled.
		now := time.Now()
		if uerr := i.DB.Model(&Installation{}).
			Where("organization_id = ? AND addon_key = ?", row.OrganizationID, row.AddonKey).
			Updates(map[string]any{"status": "disabled", "disabled_at": now}).Error; uerr != nil {
			errs = append(errs, fmt.Errorf("mark revoked %s disabled: %w", row.AddonKey, uerr))
		}
		i.setLastError(row.OrganizationID, row.AddonKey, fmt.Sprintf("revoked: %s", rev.Reason))
		slog.Warn("installer.revoked_addon_disabled", "addon", row.AddonKey, "version", row.Version, "org", row.OrganizationID.String(), "reason", rev.Reason)
		out = append(out, res)
	}
	return out, errors.Join(errs...)
}

// setLastError records a short diagnostic on the installation row. Best-effort:
// hosts whose table predates the last_error column keep working.
func (i *Installer) setLastError(orgID uuid.UUID, addonKey, msg string) {
	msg = truncateErr(msg)
	_ = i.DB.Session(&gorm.Session{Logger: logger.Discard}).Model(&Installation{}).
		Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Update("last_error", msg).Error
}
