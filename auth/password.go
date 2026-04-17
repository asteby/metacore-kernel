package auth

import "golang.org/x/crypto/bcrypt"

// DefaultBcryptCost is used when Config.BcryptCost is zero.
const DefaultBcryptCost = 10

// HashPassword returns a bcrypt hash of plain using cost. If cost is <= 0
// DefaultBcryptCost is used.
func HashPassword(plain string, cost int) (string, error) {
	if cost <= 0 {
		cost = DefaultBcryptCost
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

// CheckPassword reports whether plain matches the bcrypt hash. Returns
// false on any error (including mismatch).
func CheckPassword(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
