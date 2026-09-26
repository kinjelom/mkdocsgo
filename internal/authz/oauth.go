package authz

// The built-in OAuth 2.1 authorization server.
//
// It exists for MCP clients that cannot be handed a static header. claude.ai,
// Claude Desktop and Claude mobile add a remote server by its URL alone and
// sign in the way the MCP specification describes: a 401 pointing at protected
// resource metadata (RFC 9728), authorization server metadata (RFC 8414),
// dynamic client registration (RFC 7591), the authorization code flow with
// PKCE, and refresh. The person signing in is one of the zone's principals,
// with the password already in mkdocsgo.yml, so there is no second list of
// users to keep.
//
// Nothing is stored at run time either. A client registration, an
// authorization code, an access token and a refresh token are each a small
// JSON document sealed under MKDOCSGO_OAUTH_KEY, so any instance holding the
// key accepts what any other issued, and a restart forgets nothing. What that
// costs - a token cannot be withdrawn on its own before it expires - is in
// docs/adr/0002-built-in-oauth-for-mcp-clients.md.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// OAuthKeyVariable names the environment variable holding the key the
// authorization server signs with. It is the one secret in the arrangement,
// and it is deliberately not in mkdocsgo.yml: that file is reviewed, committed
// and harmless to leak, and this is none of those.
const OAuthKeyVariable = "MKDOCSGO_OAUTH_KEY"

// minKeyLength is 32 characters: random ones carry far more than the 128 bits
// an HMAC key needs, and anything shorter is more likely a placeholder than a
// key.
const minKeyLength = 32

// The authorization server's endpoints. The issuer is the origin a request
// arrived at, with no path, so its RFC 8414 metadata sits at the well-known
// path itself rather than below it.
const (
	AuthorizationServerMetadataPath = "/.well-known/oauth-authorization-server"
	AuthorizePath                   = "/oauth/authorize"
	TokenPath                       = "/oauth/token"
	RegisterPath                    = "/oauth/register"
)

// OAuthPaths is every path OAuthHandler answers on.
var OAuthPaths = []string{AuthorizationServerMetadataPath, AuthorizePath, TokenPath, RegisterPath}

// MethodOAuth is the Identity.Method of a caller who signed in through the
// authorization server.
const MethodOAuth = "oauth"

// The two tokens a client holds carry a prefix, like the static ones, so that
// a leaked one is recognisable to a secret scanner and to a person.
const (
	accessTokenPrefix  = "mkda_"
	refreshTokenPrefix = "mkdr_"
)

// What each sealed value is for. Every purpose signs with its own derived key.
const (
	purposeClient  = "client"
	purposeRequest = "request"
	purposeCode    = "code"
	purposeAccess  = "access"
	purposeRefresh = "refresh"
	purposeSecret  = "secret"
)

const (
	defaultAccessTTL  = time.Hour
	defaultRefreshTTL = 30 * 24 * time.Hour
	// A code crosses the browser once and is redeemed within seconds.
	codeTTL = time.Minute
	// The sign-in form: long enough to find a password manager.
	requestTTL = 10 * time.Minute

	maxForm         = 16 << 10
	maxRegistration = 64 << 10
	maxRedirectURIs = 10
	maxClientName   = 80
)

// defaultRedirectURIs is where a sign-in may send its code when mkdocsgo.yml
// does not say. The first two are the hosted Claude apps - claude.ai, Desktop,
// mobile - which all come back through Anthropic's callback; the loopback two
// are Claude Code, which listens on a port it picks per sign-in.
var defaultRedirectURIs = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
	"http://localhost/callback",
	"http://127.0.0.1/callback",
}

// OAuthSettings tunes the authorization server. The block is optional, and so
// is every field in it: a zone with `oauth: true` works without it.
type OAuthSettings struct {
	// RedirectURIs is where the server will send a code: a client may
	// register only these. It is what stops somebody from registering their
	// own address and sending a principal a sign-in link that ends there.
	RedirectURIs    []string      `yaml:"redirect_uris"`
	AccessTokenTTL  time.Duration `yaml:"access_token_ttl"`
	RefreshTokenTTL time.Duration `yaml:"refresh_token_ttl"`

	redirects []redirectPattern
}

func (s *OAuthSettings) validate() error {
	if len(s.RedirectURIs) == 0 {
		s.RedirectURIs = append([]string(nil), defaultRedirectURIs...)
	}
	for _, raw := range s.RedirectURIs {
		pattern, err := parseRedirectPattern(raw)
		if err != nil {
			return fmt.Errorf("redirect_uris: %q: %w", raw, err)
		}
		s.redirects = append(s.redirects, pattern)
	}

	if s.AccessTokenTTL == 0 {
		s.AccessTokenTTL = defaultAccessTTL
	}
	if s.RefreshTokenTTL == 0 {
		s.RefreshTokenTTL = defaultRefreshTTL
	}
	switch {
	case s.AccessTokenTTL < time.Minute:
		return fmt.Errorf("access_token_ttl: %s is under a minute, so a client would spend its time refreshing", s.AccessTokenTTL)
	case s.RefreshTokenTTL < s.AccessTokenTTL:
		return fmt.Errorf("refresh_token_ttl: %s is shorter than access_token_ttl %s, so no refresh could ever be used",
			s.RefreshTokenTTL, s.AccessTokenTTL)
	}
	return nil
}

// redirectPattern is one entry of redirect_uris. Most are compared as exact
// strings. A loopback one belongs to a native client, which listens on a port
// it picks per sign-in, so the port is ignored as RFC 8252 asks - and written
// without a path, it admits any path.
type redirectPattern struct {
	uri      string
	loopback bool
	host     string
	path     string
}

func parseRedirectPattern(raw string) (redirectPattern, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return redirectPattern{}, fmt.Errorf("not an absolute URL")
	}
	if strings.Contains(raw, "#") {
		return redirectPattern{}, fmt.Errorf("a redirect URI cannot carry a fragment")
	}
	switch {
	case u.Scheme == "https":
		return redirectPattern{uri: raw}, nil
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		if u.Port() != "" {
			return redirectPattern{}, fmt.Errorf("the port of a loopback redirect is ignored - leave it out")
		}
		return redirectPattern{uri: raw, loopback: true, host: u.Hostname(), path: u.Path}, nil
	}
	return redirectPattern{}, fmt.Errorf("only https, or plain http to localhost, 127.0.0.1 or [::1]")
}

func (p redirectPattern) allows(uri string) bool {
	if !p.loopback {
		return uri == p.uri
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || u.User != nil || strings.Contains(uri, "#") {
		return false
	}
	if u.Hostname() != p.host {
		return false
	}
	return p.path == "" || p.path == "/" || u.Path == p.path
}

// sameRedirect compares the redirect URI a client registered with the one it
// asks for: exactly, except that a loopback address ignores the port.
func sameRedirect(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, errA := url.Parse(registered)
	b, errB := url.Parse(requested)
	if errA != nil || errB != nil || a.Scheme != "http" || b.Scheme != "http" {
		return false
	}
	return isLoopbackHost(a.Hostname()) && a.Hostname() == b.Hostname() &&
		a.Path == b.Path && a.RawQuery == b.RawQuery &&
		b.User == nil && !strings.Contains(requested, "#")
}

// authServer is the authorization server: one per process, shared by every
// zone that turns it on.
type authServer struct {
	sealer
	settings *OAuthSettings
	spent    spentCodes
}

// UseOAuthKey arms the authorization server for every zone with `oauth: true`.
// Until it is called those zones accept static tokens only - which is what a
// server that does not serve /mcp over HTTP wants, since nothing could use
// the rest.
//
// The key must be the same on every instance behind one address: a code
// issued by one is redeemed at whichever the load balancer picks next.
func (c *Config) UseOAuthKey(key string) error {
	if c == nil || !c.usesOAuth() {
		return nil
	}
	var names []string
	for _, name := range sortedKeys(c.Zones) {
		if c.Zones[name].OAuth {
			names = append(names, name)
		}
	}

	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return fmt.Errorf("zone %s has oauth: true, which needs $%s: at least %d random characters, the same on every instance",
			strings.Join(names, ", "), OAuthKeyVariable, minKeyLength)
	case len(key) < minKeyLength:
		return fmt.Errorf("$%s is %d characters long; it needs at least %d", OAuthKeyVariable, len(key), minKeyLength)
	}

	c.server = &authServer{sealer: sealer{key: []byte(key)}, settings: c.OAuth}
	for _, name := range names {
		c.Zones[name].server = c.server
	}
	return nil
}

// OAuthEnabled reports whether any zone signs clients in through OAuth, which
// is when OAuthHandler has anything to serve.
func (c *Config) OAuthEnabled() bool {
	return c != nil && c.server != nil
}

// OAuthHandler serves the authorization server's endpoints, at OAuthPaths.
// Like the resource metadata they are outside every zone - this is where a
// client with no credentials goes to get some - and a host whose zone does
// not use OAuth gets 404 from all of them.
func (c *Config) OAuthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zone, ok := c.Zone(c.RequestHost(r))
		if !ok || zone.server == nil {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case AuthorizationServerMetadataPath:
			c.serveServerMetadata(w, r)
		case RegisterPath:
			c.serveRegistration(w, r, zone)
		case AuthorizePath:
			c.serveAuthorization(w, r, zone)
		case TokenPath:
			c.serveToken(w, r, zone)
		default:
			http.NotFound(w, r)
		}
	})
}

// serveServerMetadata is RFC 8414 authorization server metadata.
func (c *Config) serveServerMetadata(w http.ResponseWriter, r *http.Request) {
	if allowAnyOrigin(w, r, http.MethodGet) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	issuer := c.origin(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                   issuer,
		"authorization_endpoint":   issuer + AuthorizePath,
		"token_endpoint":           issuer + TokenPath,
		"registration_endpoint":    issuer + RegisterPath,
		"response_types_supported": []string{"code"},
		"response_modes_supported": []string{"query"},
		"grant_types_supported":    []string{"authorization_code", "refresh_token"},
		// Plain PKCE protects nothing that S256 does not, and a client that
		// cannot hash is not one this server needs to serve.
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		// RFC 9207: the redirect names who issued the code, so a client
		// talking to several servers cannot be sent one's code as another's.
		"authorization_response_iss_parameter_supported": true,
		// No scopes_supported: a principal reads the whole zone or nothing,
		// and advertising scopes would put a choice on the consent screen
		// that changes nothing.
	})
}

// clientClaims is a registered client. The client_id is this, sealed, so
// registration stores nothing and every instance recognises every client -
// which matters, because Claude registers anew on each fresh connection.
type clientClaims struct {
	Name         string   `json:"n,omitempty"`
	RedirectURIs []string `json:"r"`
	Confidential bool     `json:"s,omitempty"`
	Issued       int64    `json:"i"`
	// Nonce makes two identical registrations two clients, as they would be
	// anywhere that stored them - and two confidential ones two secrets.
	Nonce string `json:"o"`
}

// redirect resolves the redirect URI an authorization request asks for
// against the ones the client registered.
func (c clientClaims) redirect(requested string) (string, bool) {
	if requested == "" {
		// Allowed by RFC 6749 when only one was registered, and unambiguous.
		if len(c.RedirectURIs) == 1 && !strings.HasPrefix(c.RedirectURIs[0], "http:") {
			return c.RedirectURIs[0], true
		}
		return "", false
	}
	for _, registered := range c.RedirectURIs {
		if sameRedirect(registered, requested) {
			return requested, true
		}
	}
	return "", false
}

// serveRegistration is RFC 7591 dynamic client registration.
//
// Anyone may register - that is the point of it - so what a registration may
// ask for is the policy: only the redirect URIs in the allowlist, only the
// authorization code flow. A client is public unless it asks for a secret; the
// hosted Claude apps and Claude Code both register as public clients, which
// PKCE is designed for.
func (c *Config) serveRegistration(w http.ResponseWriter, r *http.Request, zone *Zone) {
	if allowAnyOrigin(w, r, http.MethodPost) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRegistration)).Decode(&request); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body is not a JSON client registration")
		return
	}

	server := zone.server
	switch {
	case len(request.RedirectURIs) == 0:
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	case len(request.RedirectURIs) > maxRedirectURIs:
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("at most %d redirect_uris", maxRedirectURIs))
		return
	}
	for _, uri := range request.RedirectURIs {
		if !server.allowsRedirect(uri) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				fmt.Sprintf("%s is not an address this server sends codes to; oauth.redirect_uris in %s lists them", uri, DefaultFileName))
			return
		}
	}
	for _, grant := range request.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata",
				fmt.Sprintf("grant type %q is not supported: only authorization_code and refresh_token", grant))
			return
		}
	}
	for _, response := range request.ResponseTypes {
		if response != "code" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata",
				fmt.Sprintf("response type %q is not supported: only code", response))
			return
		}
	}

	method := request.TokenEndpointAuthMethod
	switch method {
	case "":
		// RFC 7591 defaults to client_secret_basic, but a client that says
		// nothing and then sends no secret is far likelier than one that
		// expects a secret it never asked for; the answer says which it got.
		method = "none"
	case "none", "client_secret_post", "client_secret_basic":
	default:
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata",
			fmt.Sprintf("token_endpoint_auth_method %q is not supported", method))
		return
	}

	claims := clientClaims{
		Name:         cleanClientName(request.ClientName),
		RedirectURIs: request.RedirectURIs,
		Confidential: method != "none",
		Issued:       time.Now().Unix(),
		Nonce:        nonce(),
	}
	clientID := server.seal(purposeClient, claims)
	response := map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        claims.Issued,
		"redirect_uris":              claims.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
	}
	if claims.Name != "" {
		response["client_name"] = claims.Name
	}
	if claims.Confidential {
		response["client_secret"] = server.clientSecret(clientID)
		response["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, response)
}

// requestClaims carries an authorization request from the sign-in form's GET
// to its POST, so the POST trusts one sealed value rather than a handful of
// hidden fields.
type requestClaims struct {
	Host        string `json:"h"`
	ClientID    string `json:"c"`
	RedirectURI string `json:"r"`
	State       string `json:"s,omitempty"`
	Challenge   string `json:"x"`
	Expires     int64  `json:"e"`
}

// grantClaims is who signed in, where, and for which client: the body of an
// authorization code, an access token and a refresh token alike. Which of the
// three it is follows from the key it was sealed with, not from a field.
type grantClaims struct {
	Host      string `json:"h"`
	Zone      string `json:"z"`
	Principal string `json:"p"`
	// Stamp is the principal's password fingerprint at sign-in. A new
	// password in the file signs out every client the old one let in.
	Stamp   string `json:"k"`
	Client  string `json:"c"`
	Expires int64  `json:"e"`

	// An authorization code only.
	RedirectURI string `json:"r,omitempty"`
	Challenge   string `json:"x,omitempty"`
	Nonce       string `json:"o,omitempty"`
}

func (c *Config) serveAuthorization(w http.ResponseWriter, r *http.Request, zone *Zone) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		c.startAuthorization(w, r, zone)
	case http.MethodPost:
		c.finishAuthorization(w, r, zone)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

// startAuthorization checks an authorization request and shows the sign-in
// form.
func (c *Config) startAuthorization(w http.ResponseWriter, r *http.Request, zone *Zone) {
	server := zone.server
	query := r.URL.Query()

	// Until the client and its redirect URI are known, an error can only be
	// shown here: sending it anywhere would be an open redirect.
	clientID := query.Get("client_id")
	var client clientClaims
	if !server.open(purposeClient, clientID, &client) {
		renderSignIn(w, http.StatusBadRequest, signInPage{Realm: zone.Realm,
			Error: "This sign-in link names an application this server does not know. Start again from the application."})
		return
	}
	redirect, ok := client.redirect(query.Get("redirect_uri"))
	if !ok || !server.allowsRedirect(redirect) {
		renderSignIn(w, http.StatusBadRequest, signInPage{Realm: zone.Realm,
			Error: "This sign-in link would send you somewhere the application did not register. Start again from the application."})
		return
	}

	// From here on, errors go back to the client, as RFC 6749 asks, instead
	// of stranding the person on a page they cannot act on.
	state := query.Get("state")
	fail := func(code, description string) {
		redirectTo(w, r, redirect, url.Values{
			"error": {code}, "error_description": {description}, "state": {state}, "iss": {c.origin(r)},
		})
	}
	if query.Get("response_type") != "code" {
		fail("unsupported_response_type", "only the authorization code flow is supported")
		return
	}
	challenge := query.Get("code_challenge")
	if query.Get("code_challenge_method") != "S256" || !validChallenge(challenge) {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	for _, resource := range query["resource"] {
		if !c.isResource(r, resource) {
			fail("invalid_target", "resource is not this server's MCP endpoint")
			return
		}
	}

	request := server.seal(purposeRequest, requestClaims{
		Host:        c.RequestHost(r),
		ClientID:    clientID,
		RedirectURI: redirect,
		State:       state,
		Challenge:   challenge,
		Expires:     time.Now().Add(requestTTL).Unix(),
	})
	renderSignIn(w, http.StatusOK, newSignInPage(zone, client, redirect, request))
}

// finishAuthorization takes the submitted form: the principal's name and
// password, or a refusal.
func (c *Config) finishAuthorization(w http.ResponseWriter, r *http.Request, zone *Zone) {
	server := zone.server
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		renderSignIn(w, http.StatusBadRequest, signInPage{Realm: zone.Realm, Error: "The form could not be read. Start again from the application."})
		return
	}

	var request requestClaims
	var client clientClaims
	if !server.open(purposeRequest, r.PostForm.Get("request"), &request) || request.Host != c.RequestHost(r) ||
		!server.open(purposeClient, request.ClientID, &client) || !server.allowsRedirect(request.RedirectURI) {
		renderSignIn(w, http.StatusBadRequest, signInPage{Realm: zone.Realm,
			Error: "This sign-in form was not issued here. Start again from the application."})
		return
	}
	if expired(request.Expires) {
		renderSignIn(w, http.StatusBadRequest, signInPage{Realm: zone.Realm,
			Error: "This sign-in form has expired. Start again from the application."})
		return
	}

	back := url.Values{"state": {request.State}, "iss": {c.origin(r)}}
	if r.PostForm.Get("decision") == "deny" {
		record(r.Context(), &Identity{Zone: zone.Name})
		back.Set("error", "access_denied")
		back.Set("error_description", "the person signing in declined")
		redirectTo(w, r, request.RedirectURI, back)
		return
	}

	name := r.PostForm.Get("username")
	principal := zone.signIn(name, r.PostForm.Get("password"))
	if principal == nil {
		// The same bargain as a rejected Basic credential: a wrong guess
		// costs time, and an unknown name costs the same as a wrong password.
		record(r.Context(), &Identity{Zone: zone.Name})
		select {
		case <-time.After(failureDelay):
		case <-r.Context().Done():
		}
		page := newSignInPage(zone, client, request.RedirectURI, r.PostForm.Get("request"))
		page.Username = name
		page.Error = "Wrong name or password."
		// 403 rather than 401: the credentials were presented and were not
		// enough, and there is no HTTP challenge that would help.
		renderSignIn(w, http.StatusForbidden, page)
		return
	}

	record(r.Context(), &Identity{Zone: zone.Name, Principal: principal.Name, Method: MethodOAuth, Credential: "password"})
	back.Set("code", server.seal(purposeCode, grantClaims{
		Host:        request.Host,
		Zone:        zone.Name,
		Principal:   principal.Name,
		Stamp:       principal.stamp,
		Client:      clientTag(request.ClientID),
		Expires:     time.Now().Add(codeTTL).Unix(),
		RedirectURI: request.RedirectURI,
		Challenge:   request.Challenge,
		Nonce:       nonce(),
	}))
	redirectTo(w, r, request.RedirectURI, back)
}

// serveToken is the token endpoint: a code for tokens, or a refresh token for
// a new access token.
func (c *Config) serveToken(w http.ResponseWriter, r *http.Request, zone *Zone) {
	if allowAnyOrigin(w, r, http.MethodPost) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	server := zone.server
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body is not an application/x-www-form-urlencoded form")
		return
	}

	clientID, secret, viaHeader, err := clientCredentials(r)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var client clientClaims
	if !server.open(purposeClient, clientID, &client) ||
		(client.Confidential && !hmac.Equal([]byte(secret), []byte(server.clientSecret(clientID)))) {
		if viaHeader {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q`, zone.Realm))
		}
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client, or the wrong client secret")
		return
	}
	for _, resource := range r.PostForm["resource"] {
		if !c.isResource(r, resource) {
			oauthError(w, http.StatusBadRequest, "invalid_target", "resource is not this server's MCP endpoint")
			return
		}
	}

	switch grantType := r.PostForm.Get("grant_type"); grantType {
	case "authorization_code":
		c.redeemCode(w, r, zone, clientID)
	case "refresh_token":
		c.refresh(w, r, zone, clientID)
	case "":
		oauthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			fmt.Sprintf("grant type %q is not supported: only authorization_code and refresh_token", grantType))
	}
}

func (c *Config) redeemCode(w http.ResponseWriter, r *http.Request, zone *Zone, clientID string) {
	server := zone.server
	form := r.PostForm
	code := form.Get("code")

	var grant grantClaims
	switch {
	case !server.open(purposeCode, code, &grant) || grant.Host != c.RequestHost(r) || grant.Zone != zone.Name:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was not issued here")
	case expired(grant.Expires):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code has expired")
	case grant.Client != clientTag(clientID):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
	case form.Has("redirect_uri") && form.Get("redirect_uri") != grant.RedirectURI:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri differs from the one the code was sent to")
	case !verifierMatches(form.Get("code_verifier"), grant.Challenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
	case !server.spent.spend(code, time.Unix(grant.Expires, 0)):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code has already been used")
	default:
		principal := zone.current(grant)
		if principal == nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the principal is no longer allowed in, or its password has changed")
			return
		}
		session := time.Now().Add(server.settings.RefreshTokenTTL)
		server.issue(w, r, grant, principal, session)
	}
}

// refresh trades a refresh token for a new access token.
//
// The refresh token handed back is the same one: sealed and stored nowhere,
// the old one could not be withdrawn anyway, so a "new" one would promise a
// rotation that does not happen. The session it belongs to ends when it was
// always going to - a refresh does not extend it.
func (c *Config) refresh(w http.ResponseWriter, r *http.Request, zone *Zone, clientID string) {
	server := zone.server
	sealed, found := strings.CutPrefix(r.PostForm.Get("refresh_token"), refreshTokenPrefix)

	var grant grantClaims
	switch {
	case !found || !server.open(purposeRefresh, sealed, &grant) || grant.Host != c.RequestHost(r) || grant.Zone != zone.Name:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was not issued here")
	case expired(grant.Expires):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token has expired; sign in again")
	case grant.Client != clientTag(clientID):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
	default:
		principal := zone.current(grant)
		if principal == nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the principal is no longer allowed in, or its password has changed")
			return
		}
		server.issue(w, r, grant, principal, time.Unix(grant.Expires, 0))
	}
}

// issue answers the token endpoint with an access token and the refresh token
// for a session that ends at session.
func (s *authServer) issue(w http.ResponseWriter, r *http.Request, from grantClaims, principal *Principal, session time.Time) {
	now := time.Now()
	expires := now.Add(s.settings.AccessTokenTTL)
	if expires.After(session) {
		expires = session
	}
	grant := grantClaims{
		Host:      from.Host,
		Zone:      from.Zone,
		Principal: principal.Name,
		Stamp:     principal.stamp,
		Client:    from.Client,
	}
	grant.Expires = expires.Unix()
	access := accessTokenPrefix + s.seal(purposeAccess, grant)
	grant.Expires = session.Unix()
	refresh := refreshTokenPrefix + s.seal(purposeRefresh, grant)

	record(r.Context(), &Identity{Zone: from.Zone, Principal: principal.Name, Method: MethodOAuth, Credential: "oauth"})
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int64(expires.Sub(now).Round(time.Second).Seconds()),
		"refresh_token": refresh,
	})
}

// verifyAccess checks an access token presented on /mcp: sealed here, for this
// host and this zone, unexpired, and for a principal who is still a member
// with the password they signed in with.
func (s *authServer) verifyAccess(zone *Zone, token, host string) *Identity {
	sealed, found := strings.CutPrefix(token, accessTokenPrefix)
	var grant grantClaims
	if !found || !s.open(purposeAccess, sealed, &grant) {
		return nil
	}
	if grant.Host != host || grant.Zone != zone.Name || expired(grant.Expires) {
		return nil
	}
	principal := zone.current(grant)
	if principal == nil {
		return nil
	}
	return &Identity{Zone: zone.Name, Principal: principal.Name, Method: MethodOAuth, Credential: "oauth"}
}

// current returns the principal a grant names, provided the file still lets
// it into this zone with the same password. That is how a grant is withdrawn:
// take the principal out of the zone, or change its password, and restart.
func (z *Zone) current(grant grantClaims) *Principal {
	principal := z.member(grant.Principal)
	if principal == nil || principal.stamp == "" || principal.stamp != grant.Stamp {
		return nil
	}
	return principal
}

// signIn checks a name and password. The same principals, the same hashes and
// the same rule as HTTP Basic: a principal without a password cannot use a
// form.
func (z *Zone) signIn(name, password string) *Principal {
	principal := z.member(name)
	if principal == nil || principal.Password == "" || !verifyPassword(principal.Password, password) {
		return nil
	}
	return principal
}

func (s *authServer) allowsRedirect(uri string) bool {
	for _, pattern := range s.settings.redirects {
		if pattern.allows(uri) {
			return true
		}
	}
	return false
}

// clientSecret is derived from the client_id, so a confidential client's
// secret is stored nowhere either.
func (s *authServer) clientSecret(clientID string) string {
	return base64.RawURLEncoding.EncodeToString(s.sign(purposeSecret, clientID))
}

// isResource reports whether an RFC 8707 resource indicator names this
// server's MCP endpoint on the host the request arrived at. The scheme is not
// compared: behind a proxy that terminates TLS, this process cannot tell.
func (c *Config) isResource(r *http.Request, resource string) bool {
	u, err := url.Parse(resource)
	if err != nil || !u.IsAbs() || u.Fragment != "" || normaliseHost(u.Host) != c.RequestHost(r) {
		return false
	}
	switch strings.TrimSuffix(u.Path, "/") {
	case "", "/mcp":
		return true
	}
	return false
}

// clientCredentials reads the client's identity from the Authorization header
// (client_secret_basic) or from the form (client_secret_post, or a public
// client's bare client_id).
func clientCredentials(r *http.Request) (id, secret string, viaHeader bool, err error) {
	id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if user, password, ok := r.BasicAuth(); ok {
		// RFC 6749 section 2.3.1 form-encodes both before they are joined.
		user, errUser := url.QueryUnescape(user)
		password, errPassword := url.QueryUnescape(password)
		if errUser != nil || errPassword != nil {
			return "", "", true, fmt.Errorf("malformed client credentials in the Authorization header")
		}
		if id != "" && id != user {
			return "", "", true, fmt.Errorf("client_id in the body and in the Authorization header differ")
		}
		return user, password, true, nil
	}
	if id == "" {
		return "", "", false, fmt.Errorf("client_id is required")
	}
	return id, secret, false, nil
}

// clientTag stands in for a client_id inside a grant. A client_id is a sealed
// document of a few hundred bytes; a digest of it binds a grant to it just as
// firmly and keeps the tokens short.
func clientTag(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

// passwordStamp fingerprints a stored password hash. The hash is no secret -
// it is in the file - so neither is this; it only has to change when the
// password does.
func passwordStamp(hash string) string {
	sum := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(sum[:8])
}

// validChallenge accepts an S256 code challenge: the base64url of a SHA-256
// digest, 43 characters and nothing else.
func validChallenge(challenge string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil && len(raw) == sha256.Size
}

// verifierMatches is PKCE's check (RFC 7636 section 4.6).
func verifierMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// spentCodes remembers the authorization codes this instance has redeemed,
// until they would have expired anyway - a minute, so it stays small.
//
// It is per instance. Across instances what stops a replayed code is PKCE and
// that minute: a code is useless without the verifier, which never leaves the
// client that asked for it.
type spentCodes struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (s *spentCodes) spend(code string, expires time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for seen, until := range s.seen {
		if now.After(until) {
			delete(s.seen, seen)
		}
	}
	if _, used := s.seen[code]; used {
		return false
	}
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	s.seen[code] = expires
	return true
}

// nonce is 96 random bits, enough that two sealed values with otherwise equal
// contents never come out the same.
func nonce() string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func expired(unix int64) bool {
	return !time.Now().Before(time.Unix(unix, 0))
}

// cleanClientName makes a self-chosen client name fit to show on the sign-in
// page and nowhere else: no control characters, and short.
func cleanClientName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > maxClientName {
		name = string(runes[:maxClientName]) + "…"
	}
	return name
}

// redirectTo sends the browser back to the client with params added to its
// redirect URI. An empty value is left out, so an absent state stays absent.
func redirectTo(w http.ResponseWriter, r *http.Request, target string, params url.Values) {
	u, err := url.Parse(target)
	if err != nil {
		http.Error(w, "bad redirect URI", http.StatusBadRequest)
		return
	}
	query := u.Query()
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value := params.Get(key); value != "" {
			query.Set(key, value)
		}
	}
	u.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// allowAnyOrigin lets a client running in a browser read the answer, and
// answers its preflight. None of these endpoints reads a cookie, so an origin
// gains nothing by being let in that a server could not get by asking itself.
func allowAnyOrigin(w http.ResponseWriter, r *http.Request, method string) (preflight bool) {
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodOptions {
		return false
	}
	header.Set("Access-Control-Allow-Methods", method+", OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
	header.Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
	return true
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// oauthError is the error response RFC 6749 and RFC 7591 share.
func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func writeJSON(w http.ResponseWriter, status int, document any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(document)
}
