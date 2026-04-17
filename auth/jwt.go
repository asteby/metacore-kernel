package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims is the JWT claim set emitted and validated by this package.
// Keep the JSON tags stable: the frontend / other services may inspect them.
type Claims struct {
	jwt.RegisteredClaims
	UserID         uuid.UUID `json:"sub,omitempty"`
	OrganizationID uuid.UUID `json:"org,omitempty"`
	Email          string    `json:"email,omitempty"`
	Role           string    `json:"role,omitempty"`
}

// GenerateToken signs a HS256 JWT using the provided secret. It fills in
// IssuedAt and ExpiresAt using `expiry` (falling back to 24h when zero).
// Returns the signed string and the computed expiry timestamp.
func GenerateToken(claims Claims, secret []byte, expiry time.Duration) (string, time.Time, error) {
	if len(secret) == 0 {
		return "", time.Time{}, errors.New("auth: empty JWT secret")
	}
	if expiry <= 0 {
		expiry = 24 * time.Hour
	}

	now := time.Now()
	expiresAt := now.Add(expiry)

	// Populate standard claims if the caller didn't set them.
	if claims.IssuedAt == nil {
		claims.IssuedAt = jwt.NewNumericDate(now)
	}
	if claims.ExpiresAt == nil {
		claims.ExpiresAt = jwt.NewNumericDate(expiresAt)
	} else {
		expiresAt = claims.ExpiresAt.Time
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiresAt, nil
}

// GenerateTokenWithClaims signs an HS256 JWT from any jwt.Claims implementation.
// Use this when the app needs custom claim fields beyond the standard Claims
// struct (e.g. Plan, Features, Audience). The standard GenerateToken is still
// available for the common case.
//
// Unlike GenerateToken, this function does NOT auto-populate IssuedAt /
// ExpiresAt — callers should set those fields in their claims struct if needed.
// ttl is used to set ExpiresAt only when the claims implement *jwt.RegisteredClaims
// indirectly through the jwt.Claims interface and ExpiresAt is not already set;
// for simplicity, callers are expected to set those fields themselves, or pass 0
// to skip auto-population.
func GenerateTokenWithClaims(claims jwt.Claims, secret []byte, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("auth: empty JWT secret")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", err
	}
	return signed, nil
}

// ValidateTokenWithClaims parses and validates a JWT into the provided claims
// pointer. claims must be a pointer to a struct that implements jwt.Claims
// (e.g. *MarketplaceClaims). On success the struct is populated in-place.
// Expired tokens yield ErrExpiredToken; other failures yield ErrInvalidToken.
func ValidateTokenWithClaims(tokenStr string, secret []byte, claims jwt.Claims) error {
	if tokenStr == "" {
		return ErrMissingToken
	}
	parsed, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return secret, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return ErrExpiredToken
		}
		return errors.Join(ErrInvalidToken, err)
	}
	if !parsed.Valid {
		return ErrInvalidToken
	}
	return nil
}

// ValidateToken parses and validates a JWT, returning the typed Claims on
// success. Expired tokens yield ErrExpiredToken; all other validation
// failures yield ErrInvalidToken (wrapping the underlying error).
func ValidateToken(tokenStr string, secret []byte) (*Claims, error) {
	if tokenStr == "" {
		return nil, ErrMissingToken
	}

	parsed, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return secret, nil
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, errors.Join(ErrInvalidToken, err)
	}

	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}
