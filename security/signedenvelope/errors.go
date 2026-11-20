package signedenvelope

import (
	"errors"
	"fmt"

	"github.com/asteby/metacore-kernel/licensing/sigv2"
)

// Typed failure classes. Use errors.Is.
var (
	ErrMalformed      = errors.New("signedenvelope: input is malformed")
	ErrBadSignature   = errors.New("signedenvelope: signature does not verify")
	ErrUnknownKey     = errors.New("signedenvelope: unknown key id")
	ErrWrongPurpose   = errors.New("signedenvelope: signed for a different purpose")
	ErrKeyPurpose     = errors.New("signedenvelope: key is not allowed to sign this purpose")
	ErrKeyNotActive   = errors.New("signedenvelope: key is outside its validity window or revoked")
	ErrDowngrade      = errors.New("signedenvelope: legacy format refused, native envelope required")
	ErrUnsupportedAlg = errors.New("signedenvelope: unsupported algorithm")
	ErrExpired        = errors.New("signedenvelope: payload has expired")
	ErrNotYetValid    = errors.New("signedenvelope: payload is not valid yet")
	ErrNoKeys         = errors.New("signedenvelope: no usable keys for this purpose")
	ErrInvalidConfig  = errors.New("signedenvelope: invalid configuration")
)

// mapV2 translates a sigv2 error into this package's typed errors, keeping the
// original text in the chain.
func mapV2(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sigv2.ErrMalformed):
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	case errors.Is(err, sigv2.ErrBadSig):
		return ErrBadSignature
	case errors.Is(err, sigv2.ErrWrongDomain):
		return fmt.Errorf("%w: %v", ErrWrongPurpose, err)
	case errors.Is(err, sigv2.ErrDowngrade):
		return ErrDowngrade
	case errors.Is(err, sigv2.ErrUnsupported):
		return fmt.Errorf("%w: %v", ErrUnsupportedAlg, err)
	case errors.Is(err, sigv2.ErrUnknownKID):
		return fmt.Errorf("%w: %v", ErrUnknownKey, err)
	case errors.Is(err, sigv2.ErrKeyRole):
		return fmt.Errorf("%w: %v", ErrKeyPurpose, err)
	case errors.Is(err, sigv2.ErrKeyNotActive):
		return fmt.Errorf("%w: %v", ErrKeyNotActive, err)
	}
	return err
}
