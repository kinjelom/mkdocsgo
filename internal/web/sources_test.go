package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// offerHeader stands in for a zone: the tests say per request whether the
// Markdown is offered.
func offerHeader(r *http.Request) bool { return r.Header.Get("X-Offer") == "yes" }

var offered = map[string]string{"X-Offer": "yes"}

func testSources() map[string][]byte {
	return map[string][]byte{
		"index.md":         []byte("# Home\n\nWelcome.\n"),
		"guides/index.md":  []byte("---\ntitle: Guides\n---\n# Guides\n"),
		"guides/deploy.md": []byte("# Deploying\n"), // no HTML was built for it
		"orphan.md":        []byte("# Orphan\n"),    // nor for this one
	}
}

func TestMarkdownIsServedWhereItIsOffered(t *testing.T) {
	site, err := Load("../../testdata/built")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := site.OfferSources(Sources{Pages: testSources()}, offerHeader); n != 2 {
		t.Errorf("OfferSources = %d, want 2: only the pages MkDocs built", n)
	}
	h := site.Handler()

	for path, want := range map[string]string{
		"/index.md":        "# Home",
		"/guides/index.md": "title: Guides", // the source as written, front matter included
		"/guides.md":       "# Guides",      // the page's address with .md added
		"/guides/.md":      "# Guides",      // ... literally
	} {
		res := get(t, h, path, offered)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), want) {
			t.Errorf("%s: status %d, body %q", path, res.Code, res.Body.String())
			continue
		}
		if got := res.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", path, got)
		}
		if kind, _ := site.Describe(path); kind != KindMarkdown {
			t.Errorf("Describe(%s) = %q", path, kind)
		}
	}

	// A page that was not built has no source to offer: excluded or a
	// draft, its Markdown is exactly as unpublished as its HTML.
	for _, path := range []string{"/guides/deploy.md", "/orphan.md", "/partials/snippet.md"} {
		if res := get(t, h, path, offered); res.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, res.Code)
		}
	}

	// Where the zone does not offer it, a .md address is any missing page.
	if res := get(t, h, "/index.md", nil); res.Code != http.StatusNotFound {
		t.Errorf("not offered: status %d, want 404", res.Code)
	}
}

func TestABrowserIsShownTheMarkdownRatherThanMadeToSaveIt(t *testing.T) {
	site, err := Load("../../testdata/built")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	site.OfferSources(Sources{Pages: testSources()}, offerHeader)
	h := site.Handler()

	browser := get(t, h, "/index.md", map[string]string{"X-Offer": "yes", "Accept": "text/html,application/xhtml+xml,*/*;q=0.8"})
	agent := get(t, h, "/index.md", map[string]string{"X-Offer": "yes", "Accept": "*/*"})
	if got := browser.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("browser: Content-Type = %q", got)
	}
	if got := agent.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("agent: Content-Type = %q", got)
	}
	if browser.Body.String() != agent.Body.String() {
		t.Error("the two representations differ in content")
	}
	if browser.Header().Get("ETag") == agent.Header().Get("ETag") {
		t.Error("two representations share an ETag")
	}
	if !strings.Contains(agent.Header().Get("Vary"), "Accept") {
		t.Errorf("Vary = %q; a cache must know the type depends on Accept", agent.Header().Get("Vary"))
	}
}

var materialPage = `<!doctype html><html><head><title>Deploying</title></head><body>` +
	`<main class="md-main"><article class="md-content__inner md-typeset"><h1>Deploying</h1>` +
	strings.Repeat("<p>Rolling updates replace instances one at a time.</p>", 10) +
	`</article></main></body></html>`

func TestAMaterialPageGetsADownloadButtonWhereOffered(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "guides", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "guides", "deploy", "index.html"), []byte(materialPage), 0o644); err != nil {
		t.Fatal(err)
	}
	site, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	site.OfferSources(Sources{Pages: map[string][]byte{"guides/deploy.md": []byte("# Deploying\n")}}, offerHeader)
	h := site.Handler()

	with := get(t, h, "/guides/deploy/", offered)
	body := with.Body.String()
	for _, want := range []string{
		`<link rel="alternate" type="text/markdown" href="/guides/deploy.md" title="Markdown source"></head>`,
		`<article class="md-content__inner md-typeset"><a href="/guides/deploy.md" download="deploy.md"`,
		`class="md-content__button md-icon"><svg`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the offered page lacks %q:\n%s", want, body)
		}
	}

	without := get(t, h, "/guides/deploy/", nil)
	if without.Body.String() != materialPage {
		t.Error("a zone that does not offer Markdown got a changed page")
	}
	if with.Header().Get("ETag") == without.Header().Get("ETag") {
		t.Error("the page with the button has the ETag of the page without it")
	}

	// The variant is compressed like any other page.
	gz := get(t, h, "/guides/deploy/", map[string]string{"X-Offer": "yes", "Accept-Encoding": "gzip"})
	if gz.Header().Get("Content-Encoding") != "gzip" {
		t.Error("the page with the button is not served compressed")
	}

	// And its source answers at the address the button names.
	if res := get(t, h, "/guides/deploy.md", offered); res.Code != http.StatusOK || res.Body.String() != "# Deploying\n" {
		t.Errorf("/guides/deploy.md: status %d, body %q", res.Code, res.Body.String())
	}
}

func TestDownloadNames(t *testing.T) {
	for rel, want := range map[string]string{
		"guides/deploy.md": "deploy.md",
		"guides/index.md":  "guides.md",
		"guides/README.md": "guides.md",
		"index.md":         "index.md",
	} {
		if got := downloadName(rel); got != want {
			t.Errorf("downloadName(%q) = %q, want %q", rel, got, want)
		}
	}
}

func TestBuiltIsTheRuleOfferSourcesUses(t *testing.T) {
	site, err := Load("../../testdata/built")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	built := site.Built([]string{"index.md", "guides/index.md", "guides/deploy.md", "orphan.md", "guides/README.md"})
	for rel, want := range map[string]bool{
		"index.md":         true,
		"guides/index.md":  true,
		"guides/deploy.md": false, // docs_dir has it, the build does not
		"orphan.md":        false,
		"guides/README.md": false, // shadowed by guides/index.md, as MkDocs does it
	} {
		if built[rel] != want {
			t.Errorf("Built[%s] = %v, want %v", rel, built[rel], want)
		}
	}
}

// sectionsOf is a Sources.Section over a fixed table, standing in for the
// project's own sections.
func sectionsOf(table map[string]map[string]string) func(string, string) (string, []string, bool) {
	return func(path, anchor string) (string, []string, bool) {
		sections, ok := table[path]
		if !ok {
			return "", nil, false
		}
		if markdown, ok := sections[anchor]; ok {
			return markdown, nil, true
		}
		var anchors []string
		for a := range sections {
			if a != "" {
				anchors = append(anchors, a)
			}
		}
		return "", anchors, false
	}
}

func TestASectionIsServedOnItsOwn(t *testing.T) {
	site, err := Load("../../testdata/built")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	site.OfferSources(Sources{
		Pages: map[string][]byte{"guides/index.md": []byte("Intro.\n\n## Install\n\nRun it.\n")},
		Section: sectionsOf(map[string]map[string]string{
			"guides/index.md": {"": "Intro.\n", "install": "## Install\n\nRun it.\n"},
		}),
	}, offerHeader)
	h := site.Handler()

	for target, want := range map[string]string{
		"/guides.md?section=install":          "## Install\n\nRun it.\n",
		"/guides/index.md?section=%23install": "## Install\n\nRun it.\n", // #install, as copied from a URL
		"/guides.md?section=":                 "Intro.\n",                // the text before the first heading
		"/guides.md":                          "Intro.\n\n## Install\n\nRun it.\n",
	} {
		res := get(t, h, target, offered)
		if res.Code != http.StatusOK || res.Body.String() != want {
			t.Errorf("%s: status %d, body %q, want %q", target, res.Code, res.Body.String(), want)
		}
	}

	missing := get(t, h, "/guides.md?section=uninstall", offered)
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "install") {
		t.Errorf("missing section: status %d, body %q - want a 404 that names the anchors there are", missing.Code, missing.Body.String())
	}

	browser := get(t, h, "/guides.md?section=install", map[string]string{"X-Offer": "yes", "Accept": "text/html"})
	if got := browser.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("a browser asking for a section: Content-Type = %q", got)
	}
	if browser.Header().Get("ETag") == get(t, h, "/guides.md?section=install", offered).Header().Get("ETag") {
		t.Error("the two representations of a section share an ETag")
	}

	if res := get(t, h, "/guides.md?section=install", nil); res.Code != http.StatusNotFound {
		t.Errorf("not offered: status %d, want 404", res.Code)
	}
}
