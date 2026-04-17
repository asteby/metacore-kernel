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
