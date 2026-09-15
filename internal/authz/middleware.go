package authz

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MetadataPath is where RFC 9728 protected resource metadata is served. The
// suffixed form is what a client derives from the resource https://host/mcp;
// the bare form is what a client that ignores the path asks for. Both answer,
// because guessing which one an agent implements is not a game worth playing.
const (
	MetadataPath    = "/.well-known/oauth-protected-resource"
	MetadataPathMCP = "/.well-known/oauth-protected-resource/mcp"
)

// failureDelay is paid by every rejected credential. It slows a password guess
// to a crawl, and it flattens the timing difference between "no such
// principal" and "wrong password" - which is why there is no dummy hash
// computed for an unknown user, an approach that would hand an anonymous
// caller a way to spend the server's memory on demand.
// It is a variable only so the tests need not wait for it.
var failureDelay = 300 * time.Millisecond

// Middleware applies the zone policy for one surface.
//
// A nil Config is the project that has no mkdocsgo.yml: the handler is
// returned untouched, and the server behaves exactly as it did before zones
// existed.
func (c *Config) Middleware(surface Surface, next http.Handler) http.Handler {
	if c == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zone, ok := c.Zone(c.RequestHost(r))
		if !ok {
			// Fail closed. An address nobody configured is far more likely to
			// be a stale DNS record or a probe than a zone someone forgot,
			// and the alternative - serving it - is the one mistake a zone
			// file exists to prevent.
			deny(w, "this address is not configured to serve documentation")
			return
		}
		if zone.Access == AccessOff {
			record(r.Context(), &Identity{Zone: zone.Name})
			deny(w, "this address is out of service")
			return
		}

		if !zone.Restricted() {
			next.ServeHTTP(w, withIdentity(r, &Identity{Zone: zone.Name}))
			return
		}

		identity, presented := zone.authenticate(surface, r)
		if identity == nil {
			// The zone is logged even when nobody got in: a run of 401s
			// against one zone is the thing an audit wants to see.
			record(r.Context(), &Identity{Zone: zone.Name})
			// The delay is for a wrong guess. A first request carries no
			// credentials at all - every browser visit begins that way - and
			// there is nothing to slow down about asking for them.
			if presented {
				select {
				case <-time.After(failureDelay):
				case <-r.Context().Done():
				}
			}
			c.challenge(w, r, zone, surface)
			return
		}

		if surface == SurfaceSite {
			// The cache policy in internal/web is written for a public site:
			// fingerprinted assets are `public, immutable`, which is exactly
			// what a shared cache would be entitled to keep and hand to the
			// next person. Nothing else about the policy changes.
			w = &privateWriter{ResponseWriter: w}
		}
		next.ServeHTTP(w, withIdentity(r, identity))
	})
}

// withIdentity makes the identity reachable both below the zone check, through
// the request context, and above it, through the recorder the access log left
// in that context.
func withIdentity(r *http.Request, id *Identity) *http.Request {
	record(r.Context(), id)
	return r.WithContext(WithIdentity(r.Context(), id))
}

// authenticate returns who the caller is, or nil, and whether they presented
// anything at all. It never says why it refused, because the caller is told the
// same thing either way.
func (z *Zone) authenticate(surface Surface, r *http.Request) (*Identity, bool) {
	header := r.Header.Get("Authorization")
	if strings.TrimSpace(header) == "" {
		return nil, false
	}
	scheme, credentials, _ := strings.Cut(header, " ")
	credentials = strings.TrimSpace(credentials)

	switch {
	case surface == SurfaceMCP && strings.EqualFold(scheme, "bearer"):
		return z.verifyBearer(credentials), true
	case surface == SurfaceSite && strings.EqualFold(scheme, "basic"):
		return z.verifyBasic(credentials), true
	}
	return nil, true
}

func (z *Zone) verifyBasic(encoded string) *Identity {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	name, password, found := strings.Cut(string(raw), ":")
	if !found {
		return nil
	}
	for _, principal := range z.members {
		if principal.Name != name || principal.Password == "" {
			continue
		}
		if verifyPassword(principal.Password, password) {
			return &Identity{Zone: z.Name, Principal: principal.Name, Method: z.Method, Credential: "password"}
		}
		return nil
	}
	return nil
}

func (z *Zone) verifyBearer(secret string) *Identity {
	if secret == "" {
		return nil
	}
	now := time.Now().UTC()
	// Every token of every member is compared, with no early exit on a match,
	// so the time taken says nothing about which principals exist or how their
	// tokens are ordered in the file.
	var found *Identity
	for _, principal := range z.members {
		for _, token := range principal.Tokens {
			if !verifyToken(token.Hash, secret) {
				continue
			}
			if !token.expires.IsZero() && !now.Before(token.expires) {
				continue
			}
			found = &Identity{
				Zone:       z.Name,
				Principal:  principal.Name,
				Method:     z.Method,
				Credential: token.ID,
			}
		}
	}
	return found
}

// challenge is the 401. On /mcp it is the challenge the MCP specification asks
// for: a bearer scheme and a pointer to this server's protected resource
// metadata. Today that document says the tokens are issued out of band; when
// Keycloak arrives it will name the authorization server instead, and a client
// that already follows the pointer needs no change.
func (c *Config) challenge(w http.ResponseWriter, r *http.Request, zone *Zone, surface Surface) {
	if surface == SurfaceMCP {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(
			`Bearer realm=%q, resource_metadata=%q`, zone.Realm, c.metadataURL(r)))
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "authentication required: present a bearer token", http.StatusUnauthorized)
		return
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q, charset="UTF-8"`, zone.Realm))
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

func deny(w http.ResponseWriter, message string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, message, http.StatusForbidden)
}

// Metadata serves RFC 9728 protected resource metadata for whichever zone the
// request arrived at. It is never behind the zone policy: a client reads it in
// order to learn how to authenticate.
func (c *Config) Metadata() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zone, ok := c.Zone(c.RequestHost(r))
		if !ok || !zone.Restricted() {
			http.NotFound(w, r)
			return
		}
		document := map[string]any{
			"resource":                 c.resourceURL(r),
			"resource_name":            zone.Realm,
			"bearer_methods_supported": []string{"header"},
		}
		// No authorization_servers: with `method: static` there is no OAuth
		// server to point at, and RFC 9728 makes the field optional precisely
		// for a resource whose tokens are issued out of band. The field
		// appears when a zone moves to `method: oidc`.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(document)
	})
}

func (c *Config) metadataURL(r *http.Request) string {
	return c.origin(r) + MetadataPathMCP
}

func (c *Config) resourceURL(r *http.Request) string {
	return c.origin(r) + "/mcp"
}

// origin reconstructs the address the client used. TLS is terminated in front
// of this server in every deployment it has, so the scheme cannot be read off
// the connection: https is the default, http only for a loopback address,
// which is the development case.
func (c *Config) origin(r *http.Request) string {
	authority := r.Host
	scheme := "https"
	if c != nil && c.TrustForwardedHost {
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			authority = strings.TrimSpace(first)
		}
		if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			scheme = strings.ToLower(strings.TrimSpace(first))
		}
	} else if isLoopbackHost(c.RequestHost(r)) {
		scheme = "http"
	}
	return scheme + "://" + authority
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// privateWriter downgrades a public cache policy to a private one, and marks
// the response as something search engines must not index. It rewrites at
// WriteHeader because that is the last moment the handler underneath can still
// have changed its mind about either header.
type privateWriter struct {
	http.ResponseWriter
	written bool
}

func (p *privateWriter) WriteHeader(status int) {
	p.rewrite()
	p.ResponseWriter.WriteHeader(status)
}

func (p *privateWriter) Write(b []byte) (int, error) {
	p.rewrite()
	return p.ResponseWriter.Write(b)
}

func (p *privateWriter) Flush() {
	if flusher, ok := p.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (p *privateWriter) rewrite() {
	if p.written {
		return
	}
	p.written = true
	header := p.Header()
	header.Set("X-Robots-Tag", "noindex, nofollow")

	cache := header.Get("Cache-Control")
	switch {
	case cache == "":
		header.Set("Cache-Control", "private, no-cache")
	case strings.HasPrefix(cache, "public, "):
		// Keep the freshness the site chose - a fingerprinted asset is still
		// immutable - and take away the right to store it in a shared cache.
		header.Set("Cache-Control", "private, "+strings.TrimPrefix(cache, "public, "))
	case strings.Contains(cache, "private"):
	default:
		header.Set("Cache-Control", "private, "+cache)
	}
}
