package auth

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("s3cret!", 4) // low cost for test speed
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if !CheckPassword(hash, "s3cret!") {
		t.Error("CheckPassword should accept correct password")
	}
	if CheckPassword(hash, "wrong") {
		t.Error("CheckPassword should reject wrong password")
	}
}

func TestCheckPassword_EmptyInputs(t *testing.T) {
	if CheckPassword("", "x") {
		t.Error("empty hash should not match")
	}
	if CheckPassword("x", "") {
		t.Error("empty plain should not match")
	}
}

func TestHashPassword_DefaultCostWhenZero(t *testing.T) {
	hash, err := HashPassword("abc", 0)
	if err != nil {
		t.Fatalf("HashPassword(cost=0): %v", err)
	}
	if !CheckPassword(hash, "abc") {
		t.Error("default-cost hash should verify")
	}
}
