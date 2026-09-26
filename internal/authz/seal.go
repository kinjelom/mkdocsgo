package authz

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// maxSealed bounds what open will look at. Everything this package seals is a
// few hundred bytes; anything much longer did not come from here, and is not
// worth an HMAC to find that out.
const maxSealed = 4096

// sealer hands state to a client and takes it back later without storing it:
// a JSON document, base64url-encoded, followed by an HMAC-SHA256 over it.
//
// The document is readable by whoever holds it - there is nothing secret in a
// principal's name or a redirect URI - but it cannot be changed or forged
// without the key. Each purpose signs with its own key derived from the one
// in the environment, so an authorization code can never be presented as an
// access token, nor a client_id as either, however alike their contents look.
type sealer struct{ key []byte }

func (s sealer) seal(purpose string, claims any) string {
	// The claims are structs of strings, integers and booleans, which always
	// marshal.
	payload, _ := json.Marshal(claims)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(s.sign(purpose, encoded))
}

// open verifies a sealed value and decodes it into claims. It says only
// whether that worked: a caller has nothing to do with the reason but refuse.
func (s sealer) open(purpose, sealed string, claims any) bool {
	if len(sealed) > maxSealed {
		return false
	}
	encoded, signature, found := strings.Cut(sealed, ".")
	if !found {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(got, s.sign(purpose, encoded)) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	return json.Unmarshal(payload, claims) == nil
}

func (s sealer) sign(purpose, encoded string) []byte {
	mac := hmac.New(sha256.New, s.subkey(purpose))
	mac.Write([]byte(encoded))
	return mac.Sum(nil)
}

func (s sealer) subkey(purpose string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("mkdocsgo oauth v1 " + purpose))
	return mac.Sum(nil)
}
