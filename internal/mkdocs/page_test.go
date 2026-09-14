package mkdocs

import (
	"reflect"
	"strings"
	"testing"
)

func TestSlugifyMatchesPythonMarkdown(t *testing.T) {
	cases := map[string]string{
		"Hello World":                   "hello-world",
		"Krok 1: pobierz wersję":        "krok-1-pobierz-wersje",
		"The `cf` client":               "the-cf-client",
		"Wdrożenie na Cloud Foundry":    "wdrozenie-na-cloud-foundry",
		"HTTP/2 & TLS":                  "http2-tls",
		"  spaced   out  ":              "spaced-out",
		"Relacja do `ws-adapter-adocs`": "relacja-do-ws-adapter-adocs",
		"a_b c":                         "a_b-c",
		"IMAGE_VERSION":                 "image_version",
		"_emphasised_ word":             "emphasised-word",
		"**bold** heading":              "bold-heading",
		"[Link](http://x)":              "link",
	}
	for heading, want := range cases {
		if got := Slugify(heading); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", heading, got, want)
		}
	}
}

func TestSplitIgnoresHeadingsInsideFences(t *testing.T) {
	doc := strings.Join([]string{
		"# Title",
		"",
		"Intro text.",
		"",
		"```bash",
		"# this is a shell comment, not a heading",
		"echo hi",
		"```",
		"",
		"## Real",
		"",
		"Body.",
	}, "\n")

	sections := Split(doc)
	var headings []string
	for _, s := range sections {
		headings = append(headings, s.Title)
	}
	want := []string{"Title", "Real"}
	if !reflect.DeepEqual(headings, want) {
		t.Fatalf("headings = %v, want %v", headings, want)
	}
	if !strings.Contains(sections[0].Body, "shell comment") {
		t.Error("the fenced block should stay in the first section's body")
	}
}

func TestSplitBuildsHeadingTrail(t *testing.T) {
	doc := strings.Join([]string{
		"# Guide",
		"## Setup",
		"### Windows",
		"text",
		"### Linux",
		"text",
		"## Usage",
		"text",
	}, "\n")

	got := map[string][]string{}
	for _, s := range Split(doc) {
		got[s.Title] = s.Trail
	}
	want := map[string][]string{
		"Guide":   {},
		"Setup":   {"Guide"},
		"Windows": {"Guide", "Setup"},
		"Linux":   {"Guide", "Setup"},
		"Usage":   {"Guide"},
	}
	for title, trail := range want {
		if !reflect.DeepEqual(got[title], trail) {
			t.Errorf("trail of %q = %v, want %v", title, got[title], trail)
		}
	}
}

func TestSplitDeduplicatesAnchors(t *testing.T) {
	doc := "## Notes\na\n## Notes\nb\n## Notes\nc\n"
	var anchors []string
	for _, s := range Split(doc) {
		anchors = append(anchors, s.Anchor)
	}
	want := []string{"notes", "notes_1", "notes_2"}
	if !reflect.DeepEqual(anchors, want) {
		t.Fatalf("anchors = %v, want %v", anchors, want)
	}
}

func TestSplitStripsFrontMatterAndKeepsLineNumbers(t *testing.T) {
	doc := "---\ntitle: X\n---\n# Heading\n\nbody\n"
	sections := Split(doc)
	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1", len(sections))
	}
	if sections[0].Line != 4 {
		t.Errorf("heading line = %d, want 4", sections[0].Line)
	}
	if strings.Contains(sections[0].Body, "title: X") {
		t.Error("front matter leaked into the body")
	}
}

func TestSplitKeepsLeadTextBeforeFirstHeading(t *testing.T) {
	sections := Split("Some intro.\n\n# Heading\n\nbody\n")
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}
	if sections[0].Level != 0 || sections[0].Anchor != "" {
		t.Errorf("lead section = %+v, want level 0 with no anchor", sections[0])
	}
	if !strings.Contains(sections[0].Body, "Some intro.") {
		t.Error("lead text lost")
	}
}
