// Package mcpserver exposes a loaded MkDocs project over the Model Context
// Protocol.
//
// Tools come first and resources second, on purpose. Every MCP client
// implements tools; resource support is uneven, and a resources-only server is
// invisible to a client that speaks only tools.
package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kinjelom/mkdocsgo/internal/index"
	"github.com/kinjelom/mkdocsgo/internal/mkdocs"
)

// DefaultSearchLimit is used when a caller does not ask for a specific number
// of results.
const DefaultSearchLimit = 8

// MaxSearchLimit caps what a caller may ask for, so one query cannot flood a
// context window.
const MaxSearchLimit = 50

// Options configures the MCP surface.
type Options struct {
	Version      string
	SearchLimit  int
	MaxSnippet   int
	WithResource bool
}

// Service is the project plus its index, wired to MCP handlers.
type Service struct {
	project *mkdocs.Project
	ix      *index.Index
	opts    Options
}

type docRef struct {
	page    *mkdocs.Page
	section mkdocs.Section
}

// New indexes the project and returns a service ready to be attached to a
// server.
func New(project *mkdocs.Project, opts Options) *Service {
	if opts.SearchLimit <= 0 {
		opts.SearchLimit = DefaultSearchLimit
	}
	docs := make([]*index.Doc, 0, len(project.Pages)*4)
	for i := range project.Pages {
		page := &project.Pages[i]
		for _, section := range page.Sections {
			title := strings.TrimSpace(page.Title + " " + section.Breadcrumb())
			docs = append(docs, &index.Doc{
				Ref:   docRef{page: page, section: section},
				Title: title,
				Body:  section.Body,
			})
		}
	}
	return &Service{project: project, ix: index.Build(docs), opts: opts}
}

// Sections reports how many indexed units the project produced.
func (s *Service) Sections() int { return s.ix.Len() }

// Pages reports how many Markdown files were loaded.
func (s *Service) Pages() int { return len(s.project.Pages) }

// SiteName is the project's site_name, used to title the MCP server.
func (s *Service) SiteName() string { return s.project.SiteName }

// --- Tool payloads ----------------------------------------------------------

type searchIn struct {
	Query string `json:"query" jsonschema:"words or an identifier to look for, for example 'rolling deployment' or 'InvoiceNumber'"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of sections to return; defaults to 8, capped at 50"`
}

type searchHit struct {
	URI        string  `json:"uri" jsonschema:"resource URI of the section, readable with get_section or as an MCP resource"`
	Path       string  `json:"path" jsonschema:"page path relative to docs_dir"`
	Anchor     string  `json:"anchor" jsonschema:"heading anchor within the page; empty for text before the first heading"`
	Heading    string  `json:"heading" jsonschema:"the section heading"`
	Breadcrumb string  `json:"breadcrumb" jsonschema:"navigation path to the section, outermost first"`
	Snippet    string  `json:"snippet" jsonschema:"the matching text in context"`
	Score      float64 `json:"score" jsonschema:"relevance score; comparable only within one result set"`
}

type searchOut struct {
	Query string      `json:"query"`
	Hits  []searchHit `json:"hits"`
	Total int         `json:"total" jsonschema:"number of sections returned"`
}

type pageIn struct {
	Path string `json:"path" jsonschema:"page path relative to docs_dir, for example 'guides/intro.md'; the .md extension may be omitted"`
}

type pageSection struct {
	Anchor  string `json:"anchor"`
	Heading string `json:"heading"`
	Level   int    `json:"level"`
	Line    int    `json:"line" jsonschema:"1-based line of the heading in the source file"`
}

type pageOut struct {
	Path       string        `json:"path"`
	Title      string        `json:"title"`
	URI        string        `json:"uri"`
	Breadcrumb string        `json:"breadcrumb"`
	InNav      bool          `json:"in_nav" jsonschema:"false for a page the navigation does not reference"`
	Bytes      int           `json:"bytes"`
	Sections   []pageSection `json:"sections" jsonschema:"headings in document order, for use with get_section"`
	Markdown   string        `json:"markdown" jsonschema:"the page's original Markdown source, the same text as the content block"`
}

type sectionIn struct {
	Path   string `json:"path" jsonschema:"page path relative to docs_dir"`
	Anchor string `json:"anchor" jsonschema:"heading anchor as returned by search_docs or get_page"`
}

type sectionOut struct {
	Path       string `json:"path"`
	Anchor     string `json:"anchor"`
	Heading    string `json:"heading"`
	Breadcrumb string `json:"breadcrumb"`
	Level      int    `json:"level"`
	Line       int    `json:"line"`
	URI        string `json:"uri"`
	Markdown   string `json:"markdown" jsonschema:"the section heading and the text beneath it, as original Markdown; the same text as the content block"`
}

type listIn struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"only list pages whose path starts with this, for example 'guides/'"`
}

type listEntry struct {
	Path       string `json:"path"`
	Title      string `json:"title"`
	URI        string `json:"uri"`
	Breadcrumb string `json:"breadcrumb"`
	Sections   int    `json:"sections"`
	InNav      bool   `json:"in_nav"`
}

type listOut struct {
	Site  string      `json:"site"`
	Pages []listEntry `json:"pages"`
	Total int         `json:"total"`
}

// --- Registration -----------------------------------------------------------

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

// Register adds every tool and resource to the server.
func (s *Service) Register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_docs",
		Description: "Full-text search across the documentation. Returns the best matching " +
			"sections - not whole pages - each with its navigation breadcrumb, its anchor " +
			"and the matching text in context. Start here: it is the cheapest way to find " +
			"which page answers a question. Follow up with get_section for the full text.",
		Annotations: readOnly("Search documentation"),
	}, s.searchDocs)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_page",
		Description: "Return one page as its original Markdown source, plus the list of its " +
			"headings. Use it when you need the whole page; for a single heading prefer " +
			"get_section, which returns far less text.",
		Annotations: readOnly("Read a page"),
	}, s.getPage)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_section",
		Description: "Return one section of a page - a single heading and the text beneath it - " +
			"as Markdown. This is the cheapest way to read documentation: pair it with the " +
			"path and anchor from search_docs.",
		Annotations: readOnly("Read one section"),
	}, s.getSection)

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_pages",
		Description: "List every page with its navigation breadcrumb and heading count. Use it " +
			"to learn how the documentation is organised before searching, or to check " +
			"whether a topic has a page at all.",
		Annotations: readOnly("List pages"),
	}, s.listPages)

	if !s.opts.WithResource {
		return
	}
	for i := range s.project.Pages {
		page := &s.project.Pages[i]
		server.AddResource(&mcp.Resource{
			URI:         page.URI(),
			Name:        page.Path,
			Title:       page.Title,
			Description: page.Breadcrumb(),
			MIMEType:    "text/markdown",
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI:      page.URI(),
				MIMEType: "text/markdown",
				Text:     page.Source,
			}}}, nil
		})
	}
}

// Instructions is the server-level hint handed to the model on connect.
func (s *Service) Instructions() string {
	return fmt.Sprintf(
		"Documentation for %q, %d pages split into %d sections.\n\n"+
			"Use search_docs first; it returns sections with breadcrumbs and anchors. "+
			"Then read just what you need with get_section. Reach for get_page only when "+
			"the whole page matters - pages here can be thousands of lines.\n\n"+
			"Everything is the author's original Markdown, so quoting it is safe and paths "+
			"are real files under the project's docs directory.",
		s.project.SiteName, len(s.project.Pages), s.ix.Len())
}

// --- Handlers ---------------------------------------------------------------

func (s *Service) searchDocs(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errorResult[searchOut]("query must not be empty")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = s.opts.SearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}

	hits := s.ix.Search(query, limit)
	out := searchOut{Query: query, Hits: make([]searchHit, 0, len(hits))}
	var text strings.Builder
	for _, hit := range hits {
		ref := hit.Doc.Ref.(docRef)
		uri := ref.page.URI()
		if ref.section.Anchor != "" {
			uri += "#" + ref.section.Anchor
		}
		out.Hits = append(out.Hits, searchHit{
			URI:        uri,
			Path:       ref.page.Path,
			Anchor:     ref.section.Anchor,
			Heading:    ref.section.Title,
			Breadcrumb: breadcrumbOf(ref),
			Snippet:    hit.Snippet,
			Score:      round2(hit.Score),
		})
		fmt.Fprintf(&text, "%s\n  %s\n  %s\n\n", breadcrumbOf(ref), uri, hit.Snippet)
	}
	out.Total = len(out.Hits)

	if out.Total == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("No section matches %q. Try fewer or more general words, or call list_pages to see what exists.", query)},
		}}, out, nil
	}
	// Both a readable list and the structured hits: clients that surface only
	// text still show the breadcrumbs and snippets.
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: strings.TrimRight(text.String(), "\n")},
	}}, out, nil
}

func (s *Service) getPage(ctx context.Context, _ *mcp.CallToolRequest, in pageIn) (*mcp.CallToolResult, pageOut, error) {
	page, ok := s.project.Page(in.Path)
	if !ok {
		return errorResult[pageOut](fmt.Sprintf("no page at %q. Call list_pages to see the available paths.", in.Path))
	}
	out := pageOut{
		Path:       page.Path,
		Title:      page.Title,
		URI:        page.URI(),
		Breadcrumb: page.Breadcrumb(),
		InNav:      page.InNav,
		Bytes:      len(page.Source),
		Markdown:   page.Source,
	}
	for _, section := range page.Sections {
		if section.Title == "" {
			continue
		}
		out.Sections = append(out.Sections, pageSection{
			Anchor:  section.Anchor,
			Heading: section.Title,
			Level:   section.Level,
			Line:    section.Line,
		})
	}
	// The same Markdown goes in both channels. A client that understands output
	// schemas may render structuredContent and never look at the content block,
	// so putting the text in only one of them makes the tool return nothing
	// readable on half the clients.
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: page.Source},
	}}, out, nil
}

func (s *Service) getSection(ctx context.Context, _ *mcp.CallToolRequest, in sectionIn) (*mcp.CallToolResult, sectionOut, error) {
	page, ok := s.project.Page(in.Path)
	if !ok {
		return errorResult[sectionOut](fmt.Sprintf("no page at %q. Call list_pages to see the available paths.", in.Path))
	}
	anchor := strings.TrimPrefix(strings.TrimSpace(in.Anchor), "#")
	for _, section := range page.Sections {
		if section.Anchor != anchor {
			continue
		}
		uri := page.URI()
		if anchor != "" {
			uri += "#" + anchor
		}
		heading := ""
		if section.Title != "" {
			heading = strings.Repeat("#", section.Level) + " " + section.Title + "\n\n"
		}
		markdown := heading + section.Body
		out := sectionOut{
			Path:       page.Path,
			Anchor:     section.Anchor,
			Heading:    section.Title,
			Breadcrumb: breadcrumbOf(docRef{page: page, section: section}),
			Level:      section.Level,
			Line:       section.Line,
			URI:        uri,
			Markdown:   markdown,
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: markdown},
		}}, out, nil
	}
	return errorResult[sectionOut](fmt.Sprintf(
		"page %q has no section anchored %q. Call get_page for the list of anchors.",
		page.Path, anchor))
}

func (s *Service) listPages(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
	prefix := strings.TrimPrefix(strings.TrimSpace(in.Prefix), "/")
	out := listOut{Site: s.project.SiteName}
	var text strings.Builder
	for i := range s.project.Pages {
		page := &s.project.Pages[i]
		if prefix != "" && !strings.HasPrefix(page.Path, prefix) {
			continue
		}
		out.Pages = append(out.Pages, listEntry{
			Path:       page.Path,
			Title:      page.Title,
			URI:        page.URI(),
			Breadcrumb: page.Breadcrumb(),
			Sections:   len(page.Sections),
			InNav:      page.InNav,
		})
		fmt.Fprintf(&text, "%-52s %s\n", page.Path, page.Breadcrumb())
	}
	out.Total = len(out.Pages)
	if out.Total == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("No page path starts with %q.", prefix)},
		}}, out, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: strings.TrimRight(text.String(), "\n")},
	}}, out, nil
}

// errorResult reports a caller mistake as a tool error rather than a protocol
// error: the model can read it and correct itself, which a transport-level
// failure does not allow.
func errorResult[T any](message string) (*mcp.CallToolResult, T, error) {
	var zero T
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}, zero, nil
}

// breadcrumbOf joins the navigation path to the page with the heading trail
// inside it.
//
// Repeated segments are dropped. A page whose level-1 heading restates its
// navigation title is the norm, and it produces breadcrumbs like
// "Platform Concepts > Platform Concepts > Document availability", or
// "API Reference > Overview > API Reference > ..." when the two differ in
// wording but not in meaning. Neither tells the reader anything the shorter
// form does not, and the breadcrumb exists to orient an agent cheaply.
func breadcrumbOf(ref docRef) string {
	segments := append([]string{}, ref.page.NavTrail...)
	segments = append(segments, ref.page.Title)
	segments = append(segments, ref.section.Trail...)
	if ref.section.Title != "" {
		segments = append(segments, ref.section.Title)
	}
	return joinDistinct(segments)
}

func joinDistinct(segments []string) string {
	out := make([]string, 0, len(segments))
	seen := make(map[string]bool, len(segments))
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		if seen[strings.ToLower(segment)] {
			continue
		}
		seen[strings.ToLower(segment)] = true
		out = append(out, segment)
	}
	return strings.Join(out, " > ")
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
