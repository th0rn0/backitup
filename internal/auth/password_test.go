package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("unexpected hash format: %q", hash)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("verify correct = %v, %v; want true, nil", ok, err)
	}
	ok, err = VerifyPassword("wrong password", hash)
	if err != nil || ok {
		t.Fatalf("verify wrong = %v, %v; want false, nil", ok, err)
	}
}

func TestHashSaltsDiffer(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical — salt not applied")
	}
}

func TestHashEmptyPassword(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("expected error hashing empty password")
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "notahash", "$argon2id$bad",
		"$bcrypt$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",   // wrong algo
		"$argon2id$v=19$m=1,t=1,p=1$!!!$aGFzaA",    // bad salt b64
		"$argon2id$v=19$m=x,t=1,p=1$c2FsdA$aGFzaA", // bad params
	} {
		if ok, err := VerifyPassword("x", bad); err == nil || ok {
			t.Fatalf("VerifyPassword(%q) = %v, %v; want false, error", bad, ok, err)
		}
	}
}
