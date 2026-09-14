package mkdocs

import (
	"testing"
)

const testProject = "../../testdata/site"

func load(t *testing.T) *Project {
	t.Helper()
	p, err := Load(testProject)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

func TestLoadReadsConfiguration(t *testing.T) {
	p := load(t)
	if p.SiteName != "Test Docs" {
		t.Errorf("SiteName = %q, want %q", p.SiteName, "Test Docs")
	}
	if p.DocsDir != "docs" {
		t.Errorf("DocsDir = %q, want %q", p.DocsDir, "docs")
	}
}

func TestLoadHonoursExcludeDocs(t *testing.T) {
	for _, page := range load(t).Pages {
		if page.Path == "partials/snippet.md" {
			t.Fatal("a page excluded by exclude_docs was indexed")
		}
	}
}

func TestLoadTakesTitlesAndTrailsFromNav(t *testing.T) {
	p := load(t)
	page, ok := p.Page("guides/deploy.md")
	if !ok {
		t.Fatal("guides/deploy.md not loaded")
	}
	if page.Title != "Deploying" {
		t.Errorf("Title = %q, want %q (from nav)", page.Title, "Deploying")
	}
	if got, want := page.Breadcrumb(), "Guides > Deploying"; got != want {
		t.Errorf("Breadcrumb = %q, want %q", got, want)
	}
	if !page.InNav {
		t.Error("InNav = false, want true")
	}
}

func TestLoadMarksPagesMissingFromNav(t *testing.T) {
	page, ok := load(t).Page("orphan.md")
	if !ok {
		t.Fatal("orphan.md not loaded")
	}
	if page.InNav {
		t.Error("InNav = true for a page absent from nav")
	}
	if page.Title != "Orphan page" {
		t.Errorf("Title = %q, want the first level-1 heading", page.Title)
	}
}

func TestPageLookupTolerance(t *testing.T) {
	p := load(t)
	for _, query := range []string{
		"guides/deploy.md",
		"/guides/deploy.md",
		"guides/deploy",
		"  guides/deploy.md  ",
		"guides", // resolves to guides/index.md
	} {
		if _, ok := p.Page(query); !ok {
			t.Errorf("Page(%q) not found", query)
		}
	}
	if _, ok := p.Page("nope.md"); ok {
		t.Error("Page(\"nope.md\") found something")
	}
}

func TestPageLookupRejectsTraversal(t *testing.T) {
	p := load(t)
	for _, query := range []string{"../mkdocs.yml", "../../secrets", "guides/../../mkdocs.yml"} {
		if _, ok := p.Page(query); ok {
			t.Errorf("Page(%q) escaped the docs directory", query)
		}
	}
}

func TestSectionsSkipFencedComments(t *testing.T) {
	page, _ := load(t).Page("guides/deploy.md")
	for _, s := range page.Sections {
		if s.Title == "this comment must not be read as a heading" {
			t.Fatal("a comment inside a fenced block was parsed as a heading")
		}
	}
	var anchors []string
	for _, s := range page.Sections {
		if s.Anchor != "" {
			anchors = append(anchors, s.Anchor)
		}
	}
	want := map[string]bool{"deploying": true, "rolling-updates": true, "image_version": true}
	for _, a := range anchors {
		if !want[a] {
			t.Errorf("unexpected anchor %q", a)
		}
		delete(want, a)
	}
	for missing := range want {
		t.Errorf("missing anchor %q", missing)
	}
}
