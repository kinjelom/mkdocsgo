package authz

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAPasswordVerifiesAgainstItsOwnHash(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := checkPasswordHash(hash); err != nil {
		t.Fatalf("the hash this package produced did not pass its own validation: %v", err)
	}
	if !verifyPassword(hash, "correct horse battery staple") {
		t.Error("the right password did not verify")
	}
	if verifyPassword(hash, "correct horse battery stapl") {
		t.Error("a wrong password verified")
	}
}

func TestBcryptHashesFromHtpasswdAreAccepted(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := checkPasswordHash(string(hash)); err != nil {
		t.Fatalf("checkPasswordHash: %v", err)
	}
	if !verifyPassword(string(hash), "s3cret") {
		t.Error("a bcrypt password did not verify, so an existing htpasswd file could not be moved over")
	}
	if verifyPassword(string(hash), "wrong") {
		t.Error("a wrong password verified against a bcrypt hash")
	}
}

func TestAnArgon2HashTooLargeToVerifyIsRefused(t *testing.T) {
	// Produced somewhere with somewhere else's memory budget. Verifying it
	// would take the documentation down rather than let one person in.
	const huge = "$argon2id$v=19$m=1048576,t=2,p=1$c29tZXNhbHQxMjM0NTY$c29tZWRpZ2VzdA"
	if err := checkPasswordHash(huge); err == nil {
		t.Fatal("a hash asking for 1 GiB was accepted")
	}
}

func TestATokenVerifiesAgainstItsDigest(t *testing.T) {
	secret, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !strings.HasPrefix(secret, tokenPrefix) {
		t.Errorf("token %q has no recognisable prefix; a leaked one should be greppable", secret)
	}
	if err := checkTokenHash(hash); err != nil {
		t.Fatalf("checkTokenHash: %v", err)
	}
	if !verifyToken(hash, secret) {
		t.Error("the minted token did not verify")
	}
	if verifyToken(hash, secret+"x") {
		t.Error("a modified token verified")
	}
}

func TestTwoTokensDiffer(t *testing.T) {
	first, _, _ := NewToken()
	second, _, _ := NewToken()
	if first == second {
		t.Fatal("two calls minted the same token")
	}
}

func TestATokenHashMustBeASha256Digest(t *testing.T) {
	for _, hash := range []string{"", "mkd_plaintext", "sha256:zz", "sha1:0000"} {
		if err := checkTokenHash(hash); err == nil {
			t.Errorf("%q was accepted as a token hash", hash)
		}
	}
}
