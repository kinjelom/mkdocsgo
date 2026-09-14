package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const builtSite = "../../testdata/built"

func serve(t *testing.T) http.Handler {
	t.Helper()
	site, err := Load(builtSite)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return site.Handler()
}

func get(t *testing.T, h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDirectoryURLsResolveToIndex(t *testing.T) {
	h := serve(t)
	for _, path := range []string{"/", "/guides/", "/guides"} {
		res := get(t, h, path, nil)
		if res.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, res.Code)
		}
	}
}

func TestUnknownPathServesTheProjects404Page(t *testing.T) {
	res := get(t, serve(t), "/nope/", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
	if !strings.Contains(res.Body.String(), "Not found") {
		t.Error("the project's own 404.html was not served")
	}
	if got := res.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := serve(t)
	for _, path := range []string{"/", "/missing"} {
		res := get(t, h, path, nil)
		for name, want := range SecurityHeaders {
			if got := res.Header().Get(name); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, name, got, want)
			}
		}
	}
}

func TestCachePolicyPerPath(t *testing.T) {
	h := serve(t)
	cases := map[string]string{
		"/":                                   "no-cache",
		"/assets/stylesheets/main.abc123.css": "public, max-age=31536000, immutable",
		"/assets/vendor/redoc.js":             "public, max-age=86400",
		"/assets/images/x.png":                "public, max-age=604800",
	}
	for path, want := range cases {
		if got := get(t, h, path, nil).Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control = %q, want %q", path, got, want)
		}
	}
}

func TestETagIsContentDerivedAndEnablesConditionalGet(t *testing.T) {
	h := serve(t)

	first := get(t, h, "/", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the response")
	}

	// A second load of the same directory must produce the same ETag: that is
	// what makes two instances interchangeable behind a load balancer.
	again := get(t, serve(t), "/", nil)
	if again.Header().Get("ETag") != etag {
		t.Error("ETag differs between two loads of the same content")
	}

	notModified := get(t, h, "/", map[string]string{"If-None-Match": etag})
	if notModified.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", notModified.Code)
	}
	if notModified.Body.Len() != 0 {
		t.Error("304 responses must have no body")
	}

	weak := get(t, h, "/", map[string]string{"If-None-Match": "W/" + etag})
	if weak.Code != http.StatusNotModified {
		t.Errorf("weak comparison = %d, want 304", weak.Code)
	}
}

func TestGzipServedOnlyWhenAcceptedAndWorthIt(t *testing.T) {
	h := serve(t)

	compressed := get(t, h, "/", map[string]string{"Accept-Encoding": "gzip, deflate"})
	if compressed.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("gzip was accepted but not used")
	}
	if got := compressed.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to mention Accept-Encoding", got)
	}
	reader, err := gzip.NewReader(compressed.Body)
	if err != nil {
		t.Fatalf("response is not valid gzip: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("cannot decompress: %v", err)
	}
	if !strings.Contains(string(body), "<h1>Home</h1>") {
		t.Error("decompressed body is not the page")
	}

	plain := get(t, h, "/", nil)
	if plain.Header().Get("Content-Encoding") != "" {
		t.Error("gzip was used although the client did not accept it")
	}

	// A PNG is already compressed; storing a gzip copy would waste memory.
	image := get(t, h, "/assets/images/x.png", map[string]string{"Accept-Encoding": "gzip"})
	if image.Header().Get("Content-Encoding") == "gzip" {
		t.Error("an image should not be stored gzipped")
	}

	// Below the size threshold, compression costs more than it saves.
	small := get(t, h, "/small.txt", map[string]string{"Accept-Encoding": "gzip"})
	if small.Header().Get("Content-Encoding") == "gzip" {
		t.Error("a tiny file should not be stored gzipped")
	}
}

func TestAcceptsGzipHonoursQualityZero(t *testing.T) {
	cases := map[string]bool{
		"gzip":              true,
		"gzip, deflate, br": true,
		"*":                 true,
		"gzip;q=0.8":        true,
		"gzip;q=0":          false,
		"deflate, br":       false,
		"":                  false,
		"identity":          false,
	}
	for header, want := range cases {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestOnlyReadMethodsAreAllowed(t *testing.T) {
	h := serve(t)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want \"GET, HEAD\"", got)
	}

	// 405 says the resource exists but not with that verb, so a path that is
	// not served at all must still be 404 - which is what a POST to /mcp gets
	// when the server runs in site-only mode.
	missing := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, missing)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /mcp on a site without it = %d, want 404", rec.Code)
	}
}

func TestPathTraversalCannotEscapeTheSite(t *testing.T) {
	h := serve(t)
	for _, path := range []string{"/../go.mod", "/assets/../../go.mod", "/%2e%2e/go.mod"} {
		res := get(t, h, path, nil)
		if res.Code == http.StatusOK && strings.Contains(res.Body.String(), "module ") {
			t.Errorf("GET %s escaped the site directory", path)
		}
	}
}

func TestLoadReportsMissingDirectory(t *testing.T) {
	if _, err := Load("../../testdata/definitely-not-here"); err == nil {
		t.Error("Load succeeded on a missing directory")
	} else if !strings.Contains(err.Error(), "mkdocs build") {
		t.Errorf("error should hint at `mkdocs build`, got: %v", err)
	}
}
