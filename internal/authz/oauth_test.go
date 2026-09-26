package authz

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	testKey      = "a test key that is comfortably longer than 32 characters"
	claudeReturn = "https://claude.ai/api/mcp/auth_callback"
)

// oauthFixture is a configuration with two OAuth zones around one real
// password and one real static token, and the server armed with a key.
type oauthFixture struct {
	config   *Config
	password string
	token    string
}

func newOAuthFixture(t *testing.T, settings string) oauthFixture {
	t.Helper()
	return newOAuthFixtureWith(t, settings, "open sesame")
}

func newOAuthFixtureWith(t *testing.T, settings, password string) oauthFixture {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	secret, digest, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	yaml := fmt.Sprintf(`version: 1
%s
zones:
  intranet:
    hosts: [docs.internal]
    access: public
  internet:
    hosts: [docs.example.com]
    access: restricted
    realm: "Example documentation"
    principals: [partner-a, ci-agent]
    oauth: true
  partners:
    hosts: [partners.example.com]
    access: restricted
    principals: [partner-a]
    oauth: true
principals:
  partner-a:
    password: %q
  ci-agent:
    tokens:
      - id: 2026-09
        hash: %q
`, settings, hash, digest)
	config, err := Parse([]byte(yaml), "mkdocsgo.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := config.UseOAuthKey(testKey); err != nil {
		t.Fatalf("UseOAuthKey: %v", err)
	}
	return oauthFixture{config: config, password: password, token: secret}
}

func send(t *testing.T, h http.Handler, method, host, target, contentType, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatalf("not JSON (%d): %v\n%s", rec.Code, err, rec.Body.String())
	}
	return document
}

// register is dynamic client registration the way Claude does it: a public
// client with one redirect URI.
func register(t *testing.T, h http.Handler, host string, body string) (int, map[string]any) {
	t.Helper()
	rec := send(t, h, http.MethodPost, host, RegisterPath, "application/json", body, nil)
	return rec.Code, decode(t, rec)
}

func registerClaude(t *testing.T, h http.Handler, host string) string {
	t.Helper()
	status, document := register(t, h, host, `{"client_name":"Claude","redirect_uris":["`+claudeReturn+`"],
		"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`)
	if status != http.StatusCreated {
		t.Fatalf("registration: status = %d, %v", status, document)
	}
	return document["client_id"].(string)
}

func pkce() (verifier, challenge string) {
	verifier = "the-code-verifier-is-at-least-forty-three-characters-long"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeURL(clientID, redirect, challenge, state string) string {
	return AuthorizePath + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {"https://docs.example.com/mcp"},
	}.Encode()
}

var requestField = regexp.MustCompile(`name="request" value="([^"]+)"`)

// openSignIn fetches the sign-in form and returns the sealed request it
// carries.
func openSignIn(t *testing.T, h http.Handler, host, target string) string {
	t.Helper()
	rec := send(t, h, http.MethodGet, host, target, "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in form: status = %d\n%s", rec.Code, rec.Body.String())
	}
	match := requestField.FindStringSubmatch(rec.Body.String())
	if match == nil {
		t.Fatalf("the sign-in form carries no request:\n%s", rec.Body.String())
	}
	return match[1]
}

func submit(t *testing.T, h http.Handler, host, request, user, password, decision string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"request": {request}, "username": {user}, "password": {password}, "decision": {decision}}
	return send(t, h, http.MethodPost, host, AuthorizePath, "application/x-www-form-urlencoded", form.Encode(), nil)
}

// signInAs runs the browser half of the flow and returns the code.
func signInAs(t *testing.T, f oauthFixture, host, clientID, challenge string) string {
	t.Helper()
	h := f.config.OAuthHandler()
	sealed := openSignIn(t, h, host, authorizeURL(clientID, claudeReturn, challenge, "xyz"))
	rec := submit(t, h, host, sealed, "partner-a", f.password, "allow")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sign-in: status = %d\n%s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", location)
	}
	return code
}

func tokenRequest(t *testing.T, h http.Handler, host string, form url.Values) (int, map[string]any) {
	t.Helper()
	rec := send(t, h, http.MethodPost, host, TokenPath, "application/x-www-form-urlencoded", form.Encode(), nil)
	return rec.Code, decode(t, rec)
}

func exchange(t *testing.T, h http.Handler, host, clientID, code, verifier string) (int, map[string]any) {
	t.Helper()
	return tokenRequest(t, h, host, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {claudeReturn},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {"https://docs.example.com/mcp"},
	})
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// TestClaudeSignsInAndReadsTheDocumentation walks the whole flow in the order
// claude.ai runs it, from the first 401 to a refreshed token.
func TestClaudeSignsInAndReadsTheDocumentation(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	mcp := f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok))
	const host = "docs.example.com"

	// 1. An unauthenticated call is a 401 pointing at the resource metadata.
	res := request(t, mcp, host, "/mcp", nil)
	if res.Code != http.StatusUnauthorized || !strings.Contains(res.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("status = %d, WWW-Authenticate = %q", res.Code, res.Header().Get("WWW-Authenticate"))
	}

	// 2. The resource metadata names this origin as the authorization server.
	prm := decode(t, request(t, f.config.Metadata(), host, MetadataPathMCP, nil))
	servers, _ := prm["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != "https://docs.example.com" {
		t.Fatalf("authorization_servers = %v", prm["authorization_servers"])
	}
	if prm["resource"] != "https://docs.example.com/mcp" {
		t.Errorf("resource = %v, want the MCP URL exactly as a person types it into Claude", prm["resource"])
	}

	// 3. The authorization server metadata has what Claude checks for.
	meta := decode(t, request(t, as, host, AuthorizationServerMetadataPath, nil))
	if meta["issuer"] != "https://docs.example.com" {
		t.Errorf("issuer = %v", meta["issuer"])
	}
	for field, want := range map[string]string{
		"authorization_endpoint": "https://docs.example.com" + AuthorizePath,
		"token_endpoint":         "https://docs.example.com" + TokenPath,
		"registration_endpoint":  "https://docs.example.com" + RegisterPath,
	} {
		if meta[field] != want {
			t.Errorf("%s = %v, want %s", field, meta[field], want)
		}
	}
	if fmt.Sprint(meta["code_challenge_methods_supported"]) != "[S256]" {
		t.Errorf("code_challenge_methods_supported = %v", meta["code_challenge_methods_supported"])
	}

	// 4. Registration, 5. the sign-in form, 6. the code.
	clientID := registerClaude(t, as, host)
	verifier, challenge := pkce()
	sealed := openSignIn(t, as, host, authorizeURL(clientID, claudeReturn, challenge, "state-123"))
	rec := submit(t, as, host, sealed, "partner-a", f.password, "allow")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sign-in: status = %d\n%s", rec.Code, rec.Body.String())
	}
	location, _ := url.Parse(rec.Header().Get("Location"))
	if got := location.Scheme + "://" + location.Host + location.Path; got != claudeReturn {
		t.Fatalf("redirected to %s, want %s", got, claudeReturn)
	}
	if location.Query().Get("state") != "state-123" {
		t.Errorf("state = %q, want it passed back unchanged", location.Query().Get("state"))
	}
	if location.Query().Get("iss") != "https://docs.example.com" {
		t.Errorf("iss = %q", location.Query().Get("iss"))
	}

	// 7. The code for tokens.
	status, tokens := exchange(t, as, host, clientID, location.Query().Get("code"), verifier)
	if status != http.StatusOK {
		t.Fatalf("token: status = %d, %v", status, tokens)
	}
	if tokens["token_type"] != "Bearer" || tokens["expires_in"].(float64) != 3600 {
		t.Errorf("token response = %v", tokens)
	}
	access := tokens["access_token"].(string)
	refresh := tokens["refresh_token"].(string)
	if !strings.HasPrefix(access, accessTokenPrefix) || !strings.HasPrefix(refresh, refreshTokenPrefix) {
		t.Errorf("tokens without their prefixes: %q, %q", access, refresh)
	}

	// 8. The access token opens /mcp, and the access log knows who it was.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+access)
	ctx, recorder := WithRecorder(req.Context())
	out := httptest.NewRecorder()
	mcp.ServeHTTP(out, req.WithContext(ctx))
	if out.Code != http.StatusOK {
		t.Fatalf("/mcp with the access token: status = %d", out.Code)
	}
	if got := recorder.Identity().LogFields(); got != "zone=internet principal=partner-a/oauth" {
		t.Errorf("LogFields() = %q", got)
	}

	// 9. A refresh gives a working access token.
	status, refreshed := tokenRequest(t, as, host, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID},
	})
	if status != http.StatusOK {
		t.Fatalf("refresh: status = %d, %v", status, refreshed)
	}
	if res := request(t, mcp, host, "/mcp", bearer(refreshed["access_token"].(string))); res.Code != http.StatusOK {
		t.Fatalf("/mcp with the refreshed token: status = %d", res.Code)
	}
}

func TestClaudeCodeSignsInThroughALoopbackPortOfItsChoosing(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	const host = "docs.example.com"
	const redirect = "http://localhost:53682/callback"

	status, document := register(t, as, host, `{"client_name":"Claude Code","redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"none"}`)
	if status != http.StatusCreated {
		t.Fatalf("registration of a loopback redirect: status = %d, %v", status, document)
	}
	clientID := document["client_id"].(string)
	_, challenge := pkce()

	rec := send(t, as, http.MethodGet, host, authorizeURL(clientID, redirect, challenge, "s"), "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "program on this computer") {
		t.Error("a loopback redirect is not called out on the sign-in page")
	}

	// A port other than the one registered is still that client's loopback.
	if rec := send(t, as, http.MethodGet, host, authorizeURL(clientID, "http://localhost:40000/callback", challenge, "s"), "", "", nil); rec.Code != http.StatusOK {
		t.Errorf("another loopback port: status = %d, want 200 - RFC 8252 ignores the port", rec.Code)
	}
	// Another path is not.
	if rec := send(t, as, http.MethodGet, host, authorizeURL(clientID, "http://localhost:53682/elsewhere", challenge, "s"), "", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("another loopback path: status = %d, want 400", rec.Code)
	}
}

func TestRegistrationRefusesAnAddressOutsideTheAllowlist(t *testing.T) {
	f := newOAuthFixture(t, "")
	status, document := register(t, f.config.OAuthHandler(), "docs.example.com",
		`{"client_name":"Claude","redirect_uris":["https://attacker.example/callback"]}`)
	if status != http.StatusBadRequest || document["error"] != "invalid_redirect_uri" {
		t.Fatalf("status = %d, %v - anyone may register, so where codes go must be the policy", status, document)
	}
}

func TestTheAllowlistComesFromTheFileWhenGiven(t *testing.T) {
	f := newOAuthFixture(t, `oauth:
  redirect_uris:
    - https://chat.example.org/oauth/callback`)
	as := f.config.OAuthHandler()
	if status, document := register(t, as, "docs.example.com", `{"redirect_uris":["https://chat.example.org/oauth/callback"]}`); status != http.StatusCreated {
		t.Errorf("the listed address: status = %d, %v", status, document)
	}
	if status, _ := register(t, as, "docs.example.com", `{"redirect_uris":["`+claudeReturn+`"]}`); status != http.StatusBadRequest {
		t.Errorf("a default address: status = %d, want 400 - a list in the file replaces the defaults", status)
	}
}

func TestAWrongPasswordShowsTheFormAgain(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")
	_, challenge := pkce()
	sealed := openSignIn(t, as, "docs.example.com", authorizeURL(clientID, claudeReturn, challenge, "s"))

	for _, credentials := range [][2]string{{"partner-a", "wrong"}, {"nobody", f.password}, {"ci-agent", ""}} {
		rec := submit(t, as, "docs.example.com", sealed, credentials[0], credentials[1], "allow")
		if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" {
			t.Errorf("%v: status = %d, Location = %q", credentials, rec.Code, rec.Header().Get("Location"))
		}
		if !strings.Contains(rec.Body.String(), "Wrong name or password") || !requestField.MatchString(rec.Body.String()) {
			t.Errorf("%v: the form did not come back with the error", credentials)
		}
	}
}

func TestCancellingTellsTheClient(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")
	_, challenge := pkce()
	sealed := openSignIn(t, as, "docs.example.com", authorizeURL(clientID, claudeReturn, challenge, "s"))

	rec := submit(t, as, "docs.example.com", sealed, "", "", "deny")
	location, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || location.Query().Get("error") != "access_denied" || location.Query().Get("code") != "" {
		t.Fatalf("status = %d, Location = %s", rec.Code, location)
	}
}

func TestAnAuthorizationRequestWithoutPKCEGoesBackAsAnError(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")

	target := AuthorizePath + "?" + url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {claudeReturn}, "state": {"s"},
	}.Encode()
	rec := send(t, as, http.MethodGet, "docs.example.com", target, "", "", nil)
	location, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || location.Query().Get("error") != "invalid_request" {
		t.Fatalf("status = %d, Location = %s", rec.Code, location)
	}
}

func TestAnUnknownClientIsNotRedirectedAnywhere(t *testing.T) {
	f := newOAuthFixture(t, "")
	_, challenge := pkce()
	rec := send(t, f.config.OAuthHandler(), http.MethodGet, "docs.example.com",
		authorizeURL("forged", "https://attacker.example/callback", challenge, "s"), "", "", nil)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("status = %d, Location = %q - an unverified redirect URI is an open redirect", rec.Code, rec.Header().Get("Location"))
	}
}

func TestACodeNeedsItsVerifierAndWorksOnce(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")
	verifier, challenge := pkce()
	code := signInAs(t, f, "docs.example.com", clientID, challenge)

	if status, document := exchange(t, as, "docs.example.com", clientID, code, verifier+"x"); status != http.StatusBadRequest || document["error"] != "invalid_grant" {
		t.Errorf("wrong verifier: status = %d, %v", status, document)
	}
	if status, document := exchange(t, as, "docs.example.com", clientID, code, verifier); status != http.StatusOK {
		t.Fatalf("right verifier: status = %d, %v", status, document)
	}
	if status, document := exchange(t, as, "docs.example.com", clientID, code, verifier); status != http.StatusBadRequest || document["error"] != "invalid_grant" {
		t.Errorf("the same code again: status = %d, %v", status, document)
	}
}

func TestACodeBelongsToTheClientItWasIssuedTo(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")
	other := registerClaude(t, as, "docs.example.com")
	verifier, challenge := pkce()
	code := signInAs(t, f, "docs.example.com", clientID, challenge)

	if status, document := exchange(t, as, "docs.example.com", other, code, verifier); status != http.StatusBadRequest || document["error"] != "invalid_grant" {
		t.Errorf("status = %d, %v", status, document)
	}
}

func TestATokenIsForTheHostAndZoneThatIssuedIt(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")
	verifier, challenge := pkce()
	_, tokens := exchange(t, as, "docs.example.com", clientID, signInAs(t, f, "docs.example.com", clientID, challenge), verifier)
	access := tokens["access_token"].(string)

	// partner-a is a member of both zones; the token still only opens the one
	// it was issued in, because it names the host it is for (RFC 8707).
	mcp := f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok))
	if res := request(t, mcp, "partners.example.com", "/mcp", bearer(access)); res.Code != http.StatusUnauthorized {
		t.Errorf("another zone: status = %d, want 401", res.Code)
	}
	if res := request(t, mcp, "docs.example.com", "/mcp", bearer(access)); res.Code != http.StatusOK {
		t.Errorf("its own zone: status = %d, want 200", res.Code)
	}
}

func TestAnExpiredOrForgedAccessTokenIsRefused(t *testing.T) {
	f := newOAuthFixture(t, "")
	mcp := f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok))
	principal := f.config.Principals["partner-a"]
	grant := grantClaims{Host: "docs.example.com", Zone: "internet", Principal: "partner-a", Stamp: principal.stamp,
		Expires: time.Now().Add(-time.Second).Unix()}

	expiredToken := accessTokenPrefix + f.config.server.seal(purposeAccess, grant)
	res := request(t, mcp, "docs.example.com", "/mcp", bearer(expiredToken))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expired: status = %d, want 401", res.Code)
	}
	if !strings.Contains(res.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want invalid_token so the client refreshes", res.Header().Get("WWW-Authenticate"))
	}

	grant.Expires = time.Now().Add(time.Hour).Unix()
	forged := accessTokenPrefix + (sealer{key: []byte("another key, also long enough to pass the check")}).seal(purposeAccess, grant)
	if res := request(t, mcp, "docs.example.com", "/mcp", bearer(forged)); res.Code != http.StatusUnauthorized {
		t.Errorf("sealed with another key: status = %d, want 401", res.Code)
	}

	// A code is not an access token, however alike their contents.
	code := accessTokenPrefix + f.config.server.seal(purposeCode, grant)
	if res := request(t, mcp, "docs.example.com", "/mcp", bearer(code)); res.Code != http.StatusUnauthorized {
		t.Errorf("a code presented as an access token: status = %d, want 401", res.Code)
	}
}

func TestANewPasswordSignsEveryClientOut(t *testing.T) {
	before := newOAuthFixture(t, "")
	as := before.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")
	verifier, challenge := pkce()
	_, tokens := exchange(t, as, "docs.example.com", clientID, signInAs(t, before, "docs.example.com", clientID, challenge), verifier)

	// The same key, the same principals, a new password: what a restart
	// after editing the file looks like.
	after := newOAuthFixtureWith(t, "", "a new password")
	mcp := after.config.Middleware(SurfaceMCP, http.HandlerFunc(ok))
	if res := request(t, mcp, "docs.example.com", "/mcp", bearer(tokens["access_token"].(string))); res.Code != http.StatusUnauthorized {
		t.Errorf("access token after a password change: status = %d, want 401", res.Code)
	}
	status, document := tokenRequest(t, after.config.OAuthHandler(), "docs.example.com", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {clientID},
	})
	if status != http.StatusBadRequest || document["error"] != "invalid_grant" {
		t.Errorf("refresh after a password change: status = %d, %v - Claude needs invalid_grant to ask for a new sign-in", status, document)
	}
}

func TestAConfidentialClientMustPresentItsSecret(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	status, document := register(t, as, "docs.example.com",
		`{"redirect_uris":["`+claudeReturn+`"],"token_endpoint_auth_method":"client_secret_post"}`)
	if status != http.StatusCreated || document["client_secret"] == nil {
		t.Fatalf("status = %d, %v", status, document)
	}
	clientID, secret := document["client_id"].(string), document["client_secret"].(string)
	verifier, challenge := pkce()
	code := signInAs(t, f, "docs.example.com", clientID, challenge)

	if status, document := exchange(t, as, "docs.example.com", clientID, code, verifier); status != http.StatusUnauthorized || document["error"] != "invalid_client" {
		t.Errorf("no secret: status = %d, %v", status, document)
	}

	// client_secret_basic, form-encoded inside the header as RFC 6749 says.
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {claudeReturn}, "code_verifier": {verifier}}
	rec := send(t, as, http.MethodPost, "docs.example.com", TokenPath, "application/x-www-form-urlencoded", form.Encode(), map[string]string{
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(clientID)+":"+url.QueryEscape(secret))),
	})
	if rec.Code != http.StatusOK {
		t.Errorf("with the secret: status = %d, %s", rec.Code, rec.Body.String())
	}
}

func TestStaticTokensStillWorkInAnOAuthZone(t *testing.T) {
	f := newOAuthFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok)), "docs.example.com", "/mcp", bearer(f.token))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: Claude Code and CI keep the tokens they were given", res.Code)
	}
}

func TestAZoneWithoutOAuthHasNoAuthorizationServer(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	for _, path := range OAuthPaths {
		if rec := send(t, as, http.MethodGet, "docs.internal", path, "", "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s on a public zone: status = %d, want 404", path, rec.Code)
		}
	}

	// And a restricted zone without oauth: true names none.
	plain := newFixture(t, "")
	if err := plain.config.UseOAuthKey(testKey); err != nil {
		t.Fatalf("UseOAuthKey: %v", err)
	}
	if plain.config.OAuthEnabled() {
		t.Error("OAuthEnabled() with no zone asking for it")
	}
	prm := decode(t, request(t, plain.config.Metadata(), "docs.example.com", MetadataPathMCP, nil))
	if _, found := prm["authorization_servers"]; found {
		t.Errorf("authorization_servers = %v in a zone without oauth", prm["authorization_servers"])
	}
}

func TestTheTokenEndpointAcceptsAPreflight(t *testing.T) {
	f := newOAuthFixture(t, "")
	rec := send(t, f.config.OAuthHandler(), http.MethodOptions, "docs.example.com", TokenPath, "", "", nil)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("status = %d, headers = %v", rec.Code, rec.Header())
	}
}

func TestTheSignInPageCannotBeFramed(t *testing.T) {
	f := newOAuthFixture(t, "")
	as := f.config.OAuthHandler()
	clientID := registerClaude(t, as, "docs.example.com")
	_, challenge := pkce()
	rec := send(t, as, http.MethodGet, "docs.example.com", authorizeURL(clientID, claudeReturn, challenge, "s"), "", "", nil)
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers = %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "claude.ai") {
		t.Error("the sign-in page does not say where the person will be sent")
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if strings.Contains(string(body), "<script") {
		t.Error("the sign-in page carries a script")
	}
}

func TestTheKeyIsRequiredAndLongEnough(t *testing.T) {
	config := mustParse(t, oauthZone)
	if err := config.UseOAuthKey(""); err == nil || !strings.Contains(err.Error(), OAuthKeyVariable) {
		t.Errorf("no key: error = %v, want it to name %s", err, OAuthKeyVariable)
	}
	if err := config.UseOAuthKey("short"); err == nil {
		t.Error("a five-character key was accepted")
	}
	if err := config.UseOAuthKey("  " + testKey + "\n"); err != nil {
		t.Errorf("a key with surrounding whitespace, as a file or a secret store hands it over: %v", err)
	}

	// Without a zone that uses it, no key is needed.
	if err := mustParse(t, oneToken).UseOAuthKey(""); err != nil {
		t.Errorf("no oauth zone: error = %v", err)
	}
}

// A zone that turns OAuth on, with one principal who can sign in.
const oauthZone = `version: 1
zones:
  internet:
    hosts: [docs.example.com]
    access: restricted
    oauth: true
    principals: [reader]
principals:
  reader:
    password: "$2a$04$5q0CMY1KwvW2g7xEVJvnO.VklExmrcf6E8FraZRXg2L8U0DYts0DS"
`

func TestOAuthSettingsAreChecked(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"on a public zone": {`version: 1
zones:
  intranet:
    hosts: [docs.internal]
    access: public
    oauth: true
`, "nobody is ever asked to sign in"},
		"with nobody who has a password": {oneToken[:strings.Index(oneToken, "    principals")] + "    oauth: true\n" + oneToken[strings.Index(oneToken, "    principals"):],
			"no principal of this zone has one"},
		"settings nobody uses": {"oauth:\n  access_token_ttl: 1h\n" + oneToken[len("version: 1\n"):], "no zone has oauth: true"},
		"a plain-http redirect": {"oauth:\n  redirect_uris: [\"http://chat.example.org/cb\"]\n" + oauthZone[len("version: 1\n"):],
			"only https"},
		"a loopback port": {"oauth:\n  redirect_uris: [\"http://localhost:8080/cb\"]\n" + oauthZone[len("version: 1\n"):],
			"leave it out"},
		"a refresh shorter than an access token": {"oauth:\n  access_token_ttl: 2h\n  refresh_token_ttl: 1h\n" + oauthZone[len("version: 1\n"):],
			"refresh_token_ttl"},
		"a number where a duration belongs": {"oauth:\n  access_token_ttl: 3600\n" + oauthZone[len("version: 1\n"):],
			"time.Duration"},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.HasPrefix(tc.yaml, "version: 1") {
				tc.yaml = "version: 1\n" + tc.yaml
			}
			_, err := parse(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestOAuthDurationsAreRead(t *testing.T) {
	config := mustParse(t, "version: 1\noauth:\n  access_token_ttl: 15m\n  refresh_token_ttl: 168h\n"+oauthZone[len("version: 1\n"):])
	if config.OAuth.AccessTokenTTL != 15*time.Minute || config.OAuth.RefreshTokenTTL != 7*24*time.Hour {
		t.Errorf("TTLs = %s, %s", config.OAuth.AccessTokenTTL, config.OAuth.RefreshTokenTTL)
	}
	if err := config.UseOAuthKey(testKey); err != nil {
		t.Fatalf("UseOAuthKey: %v", err)
	}
	if summary := config.Summary(); !strings.Contains(summary, "oauth") {
		t.Errorf("Summary() = %q, want the startup log to say OAuth is on", summary)
	}
}
