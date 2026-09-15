package authz

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Every rejected credential costs failureDelay in production. The tests
	// check what the answer is, not how long it took to give.
	failureDelay = 0
	m.Run()
}

// fixture builds a configuration around one real password and one real token,
// because a hash written by hand would only prove that the parser accepts it.
type fixture struct {
	config   *Config
	password string
	token    string
}

func newFixture(t *testing.T, expires string) fixture {
	t.Helper()
	const password = "open sesame"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	secret, digest, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	expiry := ""
	if expires != "" {
		expiry = "\n        expires: " + expires
	}

	yaml := fmt.Sprintf(`version: 1
zones:
  intranet:
    hosts: [docs.internal]
    access: public
  internet:
    hosts: [docs.example.com]
    access: restricted
    realm: "Example documentation"
    principals: [partner-a]
  retired:
    hosts: [old.example.com]
    access: off
principals:
  partner-a:
    display: "Partner A"
    password: %q
    tokens:
      - id: 2026-09
        hash: %q%s
`, hash, digest, expiry)

	config, err := Parse([]byte(yaml), "mkdocsgo.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return fixture{config: config, password: password, token: secret}
}

func ok(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write([]byte("content"))
}

func request(t *testing.T, h http.Handler, host, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func basic(user, password string) map[string]string {
	return map[string]string{
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password)),
	}
}

func TestNoConfigurationLeavesTheHandlerUntouched(t *testing.T) {
	var config *Config
	res := request(t, config.Middleware(SurfaceSite, http.HandlerFunc(ok)), "anything.example.com", "/", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a project without mkdocsgo.yml must serve as it always did", res.Code)
	}
}

func TestAPublicZoneServesWithoutCredentials(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceSite, http.HandlerFunc(ok)), "docs.internal", "/", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want the site's own policy left alone", got)
	}
}

func TestAnUnconfiguredAddressIsRefused(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceSite, http.HandlerFunc(ok)), "stale.example.net", "/", nil)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: an address no zone claims must fail closed", res.Code)
	}
}

func TestAZoneTurnedOffRefusesEveryone(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceSite, http.HandlerFunc(ok)), "old.example.com", "/", basic("partner-a", f.password))
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 even with good credentials", res.Code)
	}
}

func TestARestrictedSiteChallengesWithBasic(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceSite, http.HandlerFunc(ok)), "docs.example.com", "/", nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
	challenge := res.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Basic ") || !strings.Contains(challenge, `realm="Example documentation"`) {
		t.Errorf("WWW-Authenticate = %q", challenge)
	}
	if strings.Contains(res.Body.String(), "content") {
		t.Error("the page was served to an unauthenticated caller")
	}
}

func TestTheRightPasswordOpensTheSite(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceSite, http.HandlerFunc(ok)), "docs.example.com", "/", basic("partner-a", f.password))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
}

func TestAWrongPasswordOrUnknownPrincipalIsRefused(t *testing.T) {
	f := newFixture(t, "")
	handler := f.config.Middleware(SurfaceSite, http.HandlerFunc(ok))
	for _, credentials := range []map[string]string{
		basic("partner-a", "wrong"),
		basic("nobody", f.password),
		{"Authorization": "Basic not-base64"},
		{"Authorization": "Bearer " + f.token}, // right credential, wrong surface
	} {
		if res := request(t, handler, "docs.example.com", "/", credentials); res.Code != http.StatusUnauthorized {
			t.Errorf("status = %d for %v, want 401", res.Code, credentials)
		}
	}
}

func TestARestrictedMCPSurfacePointsAtItsMetadata(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok)), "docs.example.com", "/mcp", nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
	challenge := res.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer ") {
		t.Fatalf("WWW-Authenticate = %q, want a bearer challenge", challenge)
	}
	want := `resource_metadata="https://docs.example.com` + MetadataPathMCP + `"`
	if !strings.Contains(challenge, want) {
		t.Errorf("WWW-Authenticate = %q, want it to contain %s", challenge, want)
	}
}

func TestABearerTokenOpensTheMCPSurface(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok)), "docs.example.com", "/mcp",
		map[string]string{"Authorization": "Bearer " + f.token})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	f := newFixture(t, time.Now().UTC().AddDate(0, 0, -2).Format(time.DateOnly))
	res := request(t, f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok)), "docs.example.com", "/mcp",
		map[string]string{"Authorization": "Bearer " + f.token})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an expired token", res.Code)
	}
}

func TestATokenStillValidTodayIsAccepted(t *testing.T) {
	// Its last day of validity is today: an expiry date is a last day, not a
	// deadline at midnight the night before.
	f := newFixture(t, time.Now().UTC().Format(time.DateOnly))
	res := request(t, f.config.Middleware(SurfaceMCP, http.HandlerFunc(ok)), "docs.example.com", "/mcp",
		map[string]string{"Authorization": "Bearer " + f.token})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on the last day of validity", res.Code)
	}
}

func TestRestrictedResponsesAreNotSharedCacheable(t *testing.T) {
	f := newFixture(t, "")
	res := request(t, f.config.Middleware(SurfaceSite, http.HandlerFunc(ok)), "docs.example.com", "/", basic("partner-a", f.password))
	if got := res.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want the public policy downgraded to private", got)
	}
	if got := res.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("X-Robots-Tag = %q, want noindex on a page behind credentials", got)
	}
}

func TestTheAccessLogLearnsWhoCalled(t *testing.T) {
	f := newFixture(t, "")
	handler := f.config.Middleware(SurfaceSite, http.HandlerFunc(ok))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "docs.example.com"
	for name, value := range basic("partner-a", f.password) {
		req.Header.Set(name, value)
	}
	ctx, recorder := WithRecorder(req.Context())
	handler.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))

	got := recorder.Identity().LogFields()
	if got != "zone=internet principal=partner-a/password" {
		t.Errorf("LogFields() = %q", got)
	}
}

func TestMetadataDescribesTheRestrictedZoneOnly(t *testing.T) {
	f := newFixture(t, "")
	handler := f.config.Metadata()

	res := request(t, handler, "docs.example.com", MetadataPathMCP, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	var document struct {
		Resource               string   `json:"resource"`
		ResourceName           string   `json:"resource_name"`
		BearerMethodsSupported []string `json:"bearer_methods_supported"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &document); err != nil {
		t.Fatalf("the metadata is not JSON: %v", err)
	}
	if document.Resource != "https://docs.example.com/mcp" {
		t.Errorf("resource = %q", document.Resource)
	}
	if document.ResourceName != "Example documentation" {
		t.Errorf("resource_name = %q", document.ResourceName)
	}

	// A public zone has nothing to advertise.
	if res := request(t, handler, "docs.internal", MetadataPathMCP, nil); res.Code != http.StatusNotFound {
		t.Errorf("status = %d for a public zone, want 404", res.Code)
	}
}

func TestForwardedHostIsIgnoredUnlessTrusted(t *testing.T) {
	f := newFixture(t, "")
	handler := f.config.Middleware(SurfaceSite, http.HandlerFunc(ok))
	// Claiming to have arrived at the public zone must not open the restricted
	// one: otherwise the client picks its own policy.
	res := request(t, handler, "docs.example.com", "/", map[string]string{"X-Forwarded-Host": "docs.internal"})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 - X-Forwarded-Host chose the zone", res.Code)
	}

	f.config.TrustForwardedHost = true
	if res := request(t, handler, "docs.example.com", "/", map[string]string{"X-Forwarded-Host": "docs.internal"}); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the header is trusted", res.Code)
	}
}
