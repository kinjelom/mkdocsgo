package authz

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// Argon2id parameters for a password this server hashes itself.
//
// These are the OWASP second-choice parameters - 19 MiB, two passes - rather
// than the 64 MiB first choice, because the memory is allocated per
// verification inside a container sized for a documentation server: a 128 MiB
// Cloud Foundry application cannot afford two 64 MiB spikes, and a hash nobody
// can afford to verify protects nothing.
//
// A hash produced elsewhere is verified with the parameters written into it,
// not with these, so raising them later costs nothing but a re-hash.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Verifying an Argon2id hash allocates argonMemory, so an unauthenticated
// caller can otherwise turn a login form into a memory-exhaustion tool against
// a process that is also serving the documentation. Two at a time is enough
// for people reading documentation and bounded at 38 MiB.
var passwordSlots = make(chan struct{}, 2)

const tokenPrefix = "mkd_"

// HashPassword returns the PHC string to paste into mkdocsgo.yml.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("empty password")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

// NewToken mints a bearer token: the secret, which is printed once and never
// stored, and the digest that goes into the configuration.
func NewToken() (secret, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	secret = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return secret, tokenHash(secret), nil
}

// tokenHash is a plain SHA-256. A token is 256 bits of randomness, so there is
// nothing for a slow hash to defend: the offline guessing a memory-hard
// function exists to frustrate is already impossible, and making every request
// pay 19 MiB would only hand an agent's retry loop a way to exhaust the
// process.
func tokenHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func checkTokenHash(hash string) error {
	digest, found := strings.CutPrefix(hash, "sha256:")
	if !found {
		return fmt.Errorf("expected sha256:<hex>, got %q - `mkdocsgo -new-token` prints the line to paste", hash)
	}
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("not a SHA-256 digest in hex")
	}
	return nil
}

// checkPasswordHash validates the shape of a stored hash at startup, so a
// password that could never match is a failure to start rather than a 401 that
// nobody can explain.
func checkPasswordHash(encoded string) error {
	switch {
	case strings.HasPrefix(encoded, "$argon2id$"):
		_, _, _, err := decodeArgon(encoded)
		return err
	case strings.HasPrefix(encoded, "$2"):
		// htpasswd -B and every bcrypt library produce these; accepting them
		// means an existing password file can be moved over as it is.
		if _, err := bcrypt.Cost([]byte(encoded)); err != nil {
			return fmt.Errorf("not a usable bcrypt hash: %w", err)
		}
		return nil
	case encoded == "":
		return fmt.Errorf("empty")
	}
	return fmt.Errorf("unrecognised hash - expected $argon2id$... or $2y$... , not a plaintext password")
}

// verifyPassword reports whether password matches the stored hash. It is the
// only place in the package that spends real time on purpose.
func verifyPassword(encoded, password string) bool {
	switch {
	case strings.HasPrefix(encoded, "$argon2id$"):
		params, salt, want, err := decodeArgon(encoded)
		if err != nil {
			return false
		}
		passwordSlots <- struct{}{}
		got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
		<-passwordSlots
		return subtle.ConstantTimeCompare(got, want) == 1
	case strings.HasPrefix(encoded, "$2"):
		passwordSlots <- struct{}{}
		err := bcrypt.CompareHashAndPassword([]byte(encoded), []byte(password))
		<-passwordSlots
		return err == nil
	}
	return false
}

// verifyToken reports whether secret matches the stored digest.
func verifyToken(stored, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(tokenHash(secret)), []byte(stored)) == 1
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeArgon(encoded string) (argonParams, []byte, []byte, error) {
	var params argonParams
	fields := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash
	if len(fields) != 6 {
		return params, nil, nil, fmt.Errorf("malformed Argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil {
		return params, nil, nil, fmt.Errorf("malformed Argon2id version")
	}
	if version != argon2.Version {
		return params, nil, nil, fmt.Errorf("Argon2 version %d, this build understands %d", version, argon2.Version)
	}
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return params, nil, nil, fmt.Errorf("malformed Argon2id parameters")
	}
	// A hash whose parameters would allocate more than this process has was
	// produced somewhere else with somewhere else's budget; verifying it would
	// take the documentation down rather than let one person in.
	if params.memory > 128*1024 {
		return params, nil, nil, fmt.Errorf("m=%d KiB is more memory than this server will spend on one login", params.memory)
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return params, nil, nil, fmt.Errorf("malformed Argon2id salt")
	}
	sum, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(sum) == 0 {
		return params, nil, nil, fmt.Errorf("malformed Argon2id digest")
	}
	return params, salt, sum, nil
}
