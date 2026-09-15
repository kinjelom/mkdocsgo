// Package authz decides who may reach the documentation, based on the address
// they arrived at.
//
// A zone is a policy for an address, not a filter over content. One process
// serves one set of pages; a restricted zone hides nothing from someone who
// knows another zone's address. That is deliberate - runtime content filtering
// would have to rewrite the Material search index, the sitemap and the page
// set at once - so documentation that must differ is a separate build and a
// separate deployment. What zones do is separate an intranet address from an
// internet one for the same pages.
//
// Two surfaces, one policy. A browser authenticates with HTTP Basic, an agent
// with a bearer token, and both resolve to the same Identity so the access log
// says one thing. The Authenticate signature deliberately matches the shape of
// the MCP Go SDK's TokenVerifier, because the next step for the bearer half is
// a JWT verified against Keycloak's JWKS rather than a hash from a file.
//
// Nothing here is secret. The configuration holds hashes: an Argon2id PHC
// string for a password, a SHA-256 digest for a token. A leaked copy of the
// file is not a leaked credential, which is why there is no encryption layer
// to manage, rotate or lose.
package authz

import (
	"context"
	"net/http"
	"strings"
)

// Surface is which half of the server a request reached. The policy is one per
// zone; only the way credentials arrive differs.
type Surface string

const (
	// SurfaceSite is the built site at /, where a browser authenticates with
	// HTTP Basic.
	SurfaceSite Surface = "site"
	// SurfaceMCP is /mcp, where an agent presents a bearer token.
	SurfaceMCP Surface = "mcp"
)

// Identity is who made the request, as far as the zone is concerned. A request
// to a public zone still carries one, with no principal: the access log wants
// the zone either way.
type Identity struct {
	Zone       string
	Principal  string
	Method     string
	Credential string // "password", or the token's id
}

// LogFields renders the identity the way the access log prints it.
func (i *Identity) LogFields() string {
	if i == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("zone=")
	b.WriteString(i.Zone)
	if i.Principal != "" {
		b.WriteString(" principal=")
		b.WriteString(i.Principal)
		if i.Credential != "" {
			b.WriteString("/")
			b.WriteString(i.Credential)
		}
	}
	return b.String()
}

type contextKey struct{}

// WithIdentity attaches an identity to a request context.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// IdentityFrom returns the identity attached to a request, or nil when the
// request never passed through a zone - /healthz, or a server with no
// configuration at all.
func IdentityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(contextKey{}).(*Identity)
	return id
}

// RequestHost is the host a request arrived at, normalised for matching:
// lower-cased, without the port and without a trailing dot.
//
// X-Forwarded-Host is ignored unless the configuration opts in. The Cloud
// Foundry router and every Ingress controller pass the real Host through, and
// trusting a header the client can set would let the client pick its own zone.
func (c *Config) RequestHost(r *http.Request) string {
	if c != nil && c.TrustForwardedHost {
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
			// A chain of proxies appends; the first entry is the client-facing
			// name.
			if first, _, found := strings.Cut(forwarded, ","); found {
				forwarded = first
			}
			return normaliseHost(forwarded)
		}
	}
	return normaliseHost(r.Host)
}

// Recorder carries the identity of one request back out to middleware wrapped
// around the zone check - the access log, which runs first and finishes last.
// A context value cannot travel that way on its own: the zone check attaches
// its identity to a derived request that the outer handler never sees.
type Recorder struct{ identity *Identity }

// Identity is what the zone check recorded, or nil if the request never
// reached one.
func (r *Recorder) Identity() *Identity {
	if r == nil {
		return nil
	}
	return r.identity
}

type recorderKey struct{}

// WithRecorder prepares a context to collect the request's identity.
func WithRecorder(ctx context.Context) (context.Context, *Recorder) {
	recorder := &Recorder{}
	return context.WithValue(ctx, recorderKey{}, recorder), recorder
}

func record(ctx context.Context, id *Identity) {
	if recorder, ok := ctx.Value(recorderKey{}).(*Recorder); ok {
		recorder.identity = id
	}
}
