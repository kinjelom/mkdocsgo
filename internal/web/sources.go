package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
)

// KindMarkdown is what Describe calls a page's Markdown source.
const KindMarkdown = "markdown"

// Sources is the Markdown a site can offer next to its pages.
type Sources struct {
	// Pages maps a docs-relative path - "guides/deploy.md" - to its Markdown.
	Pages map[string][]byte
	// Section returns one section of a page: the heading the anchor names and
	// the text beneath it. When the page has no such section it returns false
	// and the anchors the page does have. Nil turns ?section= off.
	Section func(path, anchor string) (markdown string, anchors []string, ok bool)
}

// source is one page's Markdown, in the two types it is served as.
type source struct {
	rel      string // its path under docs_dir
	markdown *file  // text/markdown, for anything that is not a browser
	text     *file  // text/plain, so that a browser shows it rather than saving it
}

// pick is the representation a request should get.
func (src *source) pick(r *http.Request) *file {
	if wantsHTML(r) {
		return src.text
	}
	return src.markdown
}

// OfferSources makes each page's Markdown available next to the page MkDocs
// built from it: at its path under docs_dir - /guides/deploy.md - and at the
// page's own address with .md added - /guides/deploy/ as /guides/deploy.md or
// /guides/deploy/.md. ?section=<anchor> narrows any of them to one section.
// On a page built with Material, the page itself gets a download button
// beside Material's own, and every page gets a
// <link rel="alternate" type="text/markdown"> an agent can follow.
//
// A source whose page was not built - excluded, a draft - is not offered: its
// HTML is not published, so neither is its Markdown. offer decides per
// request whether any of this is visible; everywhere it says no, the site is
// exactly what MkDocs built. It returns how many pages have their source
// offered.
//
// Call it before the site serves anything.
func (s *Site) OfferSources(sources Sources, offer func(*http.Request) bool) int {
	s.offer = offer
	s.section = sources.Section
	s.sources = map[string]*source{}

	paths := make([]string, 0, len(sources.Pages))
	all := make(map[string]bool, len(sources.Pages))
	for rel := range sources.Pages {
		paths = append(paths, rel)
		all[rel] = true
	}
	sort.Strings(paths)

	type offered struct {
		src     *source
		pageURL string
	}
	var built []offered
	for _, rel := range paths {
		page, ok := s.builtPage(rel, all)
		if !ok {
			continue
		}
		content := sources.Pages[rel]
		markdown := newFile(content, ".md", page.modTime)
		markdown.content = content
		markdown.kind = KindMarkdown
		// Which of the two is served depends on Accept, and a cache has to
		// know that.
		markdown.vary = "Accept, Accept-Encoding"
		text := *markdown
		text.contentType = "text/plain; charset=utf-8"
		text.etag = strings.TrimSuffix(markdown.etag, `"`) + `-t"`
		src := &source{rel: rel, markdown: markdown, text: &text}

		// The address the button links to is the file's own path: the one
		// the repository has, and the one the MCP tools call a page by.
		canonical := "/" + rel
		s.sources[canonical] = src
		page.withSource = s.linkedVariant(page, canonical, downloadName(rel))
		built = append(built, offered{src, page.page})
	}

	// The spellings "add .md to the address" produces, after every canonical
	// path is in place, so that none of them can take a real file's path.
	for _, b := range built {
		for _, alias := range markdownAliases(b.pageURL) {
			if _, taken := s.sources[alias]; !taken {
				s.sources[alias] = b.src
			}
		}
	}
	return len(built)
}

// Built reports which of these docs-relative paths MkDocs built a page from,
// by the rule OfferSources uses - for anything else that should publish no
// more than the site does, the MCP half above all.
func (s *Site) Built(paths []string) map[string]bool {
	all := make(map[string]bool, len(paths))
	for _, rel := range paths {
		all[rel] = true
	}
	built := make(map[string]bool, len(paths))
	for _, rel := range paths {
		if _, ok := s.builtPage(rel, all); ok {
			built[rel] = true
		}
	}
	return built
}

// builtPage finds the HTML MkDocs wrote for a source: guides/deploy.md became
// guides/deploy/index.html, or guides/deploy.html without directory URLs;
// an index.md or a README.md became its directory's index.html.
//
// A plugin that moves pages elsewhere - a blog's dated URLs, a language
// prefix - defeats it, and such a page counts as not built.
func (s *Site) builtPage(rel string, all map[string]bool) (*file, bool) {
	stem := strings.TrimSuffix(rel, path.Ext(rel))
	dir, base := path.Split(stem)
	var candidates []string
	switch {
	case base == "index":
		candidates = []string{"/" + dir + "index.html"}
	case strings.EqualFold(base, "readme"):
		// MkDocs uses a README only where there is no index.md beside it.
		if all[dir+"index.md"] {
			return nil, false
		}
		candidates = []string{"/" + dir + "index.html"}
	default:
		candidates = []string{"/" + stem + "/index.html", "/" + stem + ".html"}
	}
	for _, candidate := range candidates {
		if f, ok := s.files[candidate]; ok && f.kind == KindPage {
			return f, true
		}
	}
	return nil, false
}

// markdownAliases are the addresses a page's source answers at besides its
// own path.
func markdownAliases(pageURL string) []string {
	switch {
	case pageURL == "/":
		return []string{"/index.md"}
	case strings.HasSuffix(pageURL, "/"):
		return []string{strings.TrimSuffix(pageURL, "/") + ".md", pageURL + ".md", pageURL + "index.md"}
	case strings.HasSuffix(pageURL, ".html"):
		return []string{strings.TrimSuffix(pageURL, ".html") + ".md", pageURL + ".md"}
	}
	return nil
}

// linkedVariant is the page with a way to its source: a <link> in the head
// for anything that reads HTML, and on a Material page a button among
// Material's own page actions. It is built once, here, so that serving it
// costs what serving the page costs - its own ETag and its own compressed
// copy included. Nil when the page has nowhere to put either.
func (s *Site) linkedVariant(page *file, href, filename string) *file {
	original, err := os.ReadFile(page.abs)
	if err != nil {
		return nil
	}
	content, changed := addSourceLink(original, href, filename)
	if !changed {
		return nil
	}
	variant := newFile(content, ".html", page.modTime)
	variant.content = content
	variant.abs = page.abs
	variant.kind, variant.page = page.kind, page.page
	s.gzBytes += int64(len(variant.gzipped))
	return variant
}

// materialArticle opens the content of every page Material renders; its page
// actions - edit, view source - are the first thing inside it.
var materialArticle = []byte(`<article class="md-content__inner md-typeset">`)

// addSourceLink inserts the <link> before </head> and the button after
// Material's article tag, whichever of the two the page has.
func addSourceLink(page []byte, href, filename string) ([]byte, bool) {
	href = html.EscapeString(href)
	out := page
	changed := false
	if i := bytes.Index(out, []byte("</head>")); i >= 0 {
		link := `<link rel="alternate" type="text/markdown" href="` + href + `" title="Markdown source">`
		out = splice(out, i, link)
		changed = true
	}
	if i := bytes.Index(out, materialArticle); i >= 0 {
		button := `<a href="` + href + `" download="` + html.EscapeString(filename) + `"` +
			` title="Download the Markdown source of this page" class="md-content__button md-icon">` +
			markdownIcon + `</a>`
		out = splice(out, i+len(materialArticle), button)
		changed = true
	}
	return out, changed
}

func splice(page []byte, at int, insert string) []byte {
	out := make([]byte, 0, len(page)+len(insert))
	out = append(out, page[:at]...)
	out = append(out, insert...)
	return append(out, page[at:]...)
}

// markdownIcon is a framed "M" with a downward arrow, the usual mark for
// Markdown, drawn for this server. Material colours it through currentColor,
// as it does its own page-action icons.
const markdownIcon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">` +
	`<path d="M2 5h20v14H2zm2 2v10h16V7zm2 8V9h2l1.5 2L11 9h2v6h-2v-3.2l-1.5 2-1.5-2V15zm10-6h2v3h2l-3 3.5-3-3.5h2z"/></svg>`

// downloadName is the file name a download suggests: the page's own, or for
// an index page the name of its directory, since a folder of index.md files
// helps nobody.
func downloadName(rel string) string {
	base := path.Base(rel)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if stem == "index" || strings.EqualFold(stem, "readme") {
		if dir := path.Base(path.Dir(rel)); dir != "." && dir != "/" {
			return dir + ".md"
		}
		return "index.md"
	}
	return base
}

// sectionFile is one section of a source, answered for this request. Unlike a
// whole source it is made per request - a section is small, and the pages
// times their headings would be a lot to hold for the few that are asked for.
func (s *Site) sectionFile(r *http.Request, src *source, anchor string) (*file, []string, bool) {
	markdown, anchors, ok := s.section(src.rel, anchor)
	if !ok {
		return nil, anchors, false
	}
	content := []byte(markdown)
	sum := sha256.Sum256(content)
	whole := src.pick(r)
	f := &file{
		size:        int64(len(content)),
		modTime:     whole.modTime,
		contentType: whole.contentType,
		etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
		content:     content,
		kind:        KindMarkdown,
		vary:        whole.vary,
	}
	if whole == src.text {
		f.etag = strings.TrimSuffix(f.etag, `"`) + `-t"`
	}
	return f, nil, true
}

// sectionAsked reads ?section=. Present but empty asks for the text before
// the first heading; a leading # is dropped, since that is how anchors are
// copied out of a URL.
func sectionAsked(r *http.Request) (string, bool) {
	values, asked := r.URL.Query()["section"]
	if !asked {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimSpace(values[0]), "#"), true
}

// wantsHTML reports whether a request comes from a browser navigating to the
// address, which would save text/markdown as a download rather than show it.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
