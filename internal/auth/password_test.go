package auth

import (
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format: %s", h)
	}
	ok, err := CheckPassword(h, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = CheckPassword(h, "wrong password")
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
	h2, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if h == h2 {
		t.Fatal("two hashes of the same password are equal; the salt is not random")
	}
}

func TestCheckPasswordRejectsBadHashes(t *testing.T) {
	bad := []string{
		"",
		"plain",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=99999999,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$***$aGFzaA",
	}
	for _, h := range bad {
		if _, err := CheckPassword(h, "x"); err == nil {
			t.Errorf("CheckPassword(%q) did not return an error", h)
		}
	}
}

func TestNewToken(t *testing.T) {
	a, b := NewToken("sqp_"), NewToken("sqp_")
	if a == b {
		t.Fatal("tokens are equal")
	}
	if !strings.HasPrefix(a, "sqp_") || len(a) != 4+32 {
		t.Fatalf("unexpected token %q", a)
	}
	if len(HashToken(a)) != 32 {
		t.Fatal("hash length is not 32")
	}
}
