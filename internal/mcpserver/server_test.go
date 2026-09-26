package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kinjelom/mkdocsgo/internal/mkdocs"
)

// connect wires a real client to a real server over an in-memory transport, so
// every assertion below travels the actual protocol: schema validation,
// serialisation and all.
func connect(t *testing.T) (*mcp.ClientSession, context.Context) {
	t.Helper()
	return connectWith(t, nil)
}

func connectWith(t *testing.T, observer Observer) (*mcp.ClientSession, context.Context) {
	t.Helper()

	project, err := mkdocs.Load("../../testdata/site")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	service := New(project, Options{Version: "test", WithResource: true, Observer: observer})

	server := mcp.NewServer(
		&mcp.Implementation{Name: "mkdocsgo", Version: "test"},
		&mcp.ServerOptions{Instructions: service.Instructions()},
	)
	service.Register(server)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session, ctx
}

func call(t *testing.T, session *mcp.ClientSession, ctx context.Context, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	return out
}

func TestToolsAreAdvertised(t *testing.T) {
	session, ctx := connect(t)
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := map[string]bool{}
	for _, tool := range res.Tools {
		found[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q is not marked read-only", tool.Name)
		}
	}
	for _, want := range []string{"search_docs", "get_page", "get_section", "list_pages"} {
		if !found[want] {
			t.Errorf("tool %q is missing", want)
		}
	}
}

func TestSearchFindsTheRightSection(t *testing.T) {
	session, ctx := connect(t)
	res := call(t, session, ctx, "search_docs", map[string]any{"query": "rolling deployment downtime"})
	if res.IsError {
		t.Fatalf("search failed: %s", textOf(res))
	}

	out := structured[searchOut](t, res)
	if out.Total == 0 {
		t.Fatal("search returned no hits")
	}
	top := out.Hits[0]
	if top.Path != "guides/deploy.md" || top.Anchor != "rolling-updates" {
		t.Errorf("top hit = %s#%s, want guides/deploy.md#rolling-updates", top.Path, top.Anchor)
	}
	if top.URI != "docs://guides/deploy.md#rolling-updates" {
		t.Errorf("URI = %q", top.URI)
	}
	if !strings.Contains(top.Breadcrumb, "Guides > Deploying") {
		t.Errorf("breadcrumb = %q, want the nav path in it", top.Breadcrumb)
	}
	if top.Snippet == "" {
		t.Error("hit has no snippet")
	}
	// Clients that render only text must still see something useful.
	if !strings.Contains(textOf(res), "docs://guides/deploy.md#rolling-updates") {
		t.Error("the text content does not carry the URI")
	}
}

func TestSearchLimitIsCapped(t *testing.T) {
	session, ctx := connect(t)
	res := call(t, session, ctx, "search_docs", map[string]any{"query": "the", "limit": 9999})
	out := structured[searchOut](t, res)
	if out.Total > MaxSearchLimit {
		t.Errorf("returned %d hits, want at most %d", out.Total, MaxSearchLimit)
	}
}

func TestSearchEmptyQueryIsAToolError(t *testing.T) {
	session, ctx := connect(t)
	res := call(t, session, ctx, "search_docs", map[string]any{"query": "   "})
	if !res.IsError {
		t.Error("a blank query should be reported as a tool error the model can read")
	}
}

func TestGetPageReturnsSourceAndHeadings(t *testing.T) {
	session, ctx := connect(t)
	res := call(t, session, ctx, "get_page", map[string]any{"path": "guides/deploy"})
	if res.IsError {
		t.Fatalf("get_page failed: %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "# Deploying") {
		t.Error("the page source is not in the content")
	}
	out := structured[pageOut](t, res)
	// A client that understands output schemas may render structuredContent and
	// never read the content block; the Markdown has to be in both.
	if !strings.Contains(out.Markdown, "# Deploying") {
		t.Error("the page source is not in the structured result")
	}
	if out.Markdown != textOf(res) {
		t.Error("the two channels disagree about the page source")
	}
	if out.Path != "guides/deploy.md" {
		t.Errorf("Path = %q, want the resolved path", out.Path)
	}
	var anchors []string
	for _, s := range out.Sections {
		anchors = append(anchors, s.Anchor)
	}
	for _, want := range []string{"deploying", "rolling-updates", "image_version"} {
		if !strings.Contains(strings.Join(anchors, ","), want) {
			t.Errorf("anchor %q missing from %v", want, anchors)
		}
	}
}

func TestGetSectionReturnsOnlyThatSection(t *testing.T) {
	session, ctx := connect(t)
	res := call(t, session, ctx, "get_section", map[string]any{
		"path": "guides/deploy.md", "anchor": "image_version",
	})
	if res.IsError {
		t.Fatalf("get_section failed: %s", textOf(res))
	}
	text := textOf(res)
	if !strings.Contains(text, "IMAGE_VERSION") {
		t.Error("the requested section is not in the content")
	}
	if strings.Contains(text, "rolling deployment keeps") {
		t.Error("get_section leaked a neighbouring section")
	}
	out := structured[sectionOut](t, res)
	if !strings.Contains(out.Markdown, "IMAGE_VERSION") {
		t.Error("the requested section is not in the structured result")
	}
	if strings.Contains(out.Markdown, "rolling deployment keeps") {
		t.Error("the structured result leaked a neighbouring section")
	}
	if out.Markdown != text {
		t.Error("the two channels disagree about the section text")
	}
	// A leading '#' is a natural thing for a model to send.
	again := call(t, session, ctx, "get_section", map[string]any{
		"path": "guides/deploy.md", "anchor": "#image_version",
	})
	if again.IsError {
		t.Error("a '#'-prefixed anchor should be accepted")
	}
}

func TestUnknownPageAndAnchorAreToolErrors(t *testing.T) {
	session, ctx := connect(t)
	missing := call(t, session, ctx, "get_page", map[string]any{"path": "nope.md"})
	if !missing.IsError || !strings.Contains(textOf(missing), "list_pages") {
		t.Error("an unknown page should be a readable tool error pointing at list_pages")
	}
	badAnchor := call(t, session, ctx, "get_section", map[string]any{
		"path": "guides/deploy.md", "anchor": "does-not-exist",
	})
	if !badAnchor.IsError || !strings.Contains(textOf(badAnchor), "get_page") {
		t.Error("an unknown anchor should be a readable tool error pointing at get_page")
	}
}

func TestListPagesRespectsPrefixAndHidesExcluded(t *testing.T) {
	session, ctx := connect(t)

	all := structured[listOut](t, call(t, session, ctx, "list_pages", map[string]any{}))
	if all.Total == 0 {
		t.Fatal("list_pages returned nothing")
	}
	for _, page := range all.Pages {
		if strings.HasPrefix(page.Path, "partials/") {
			t.Error("an excluded page is listed")
		}
	}

	guides := structured[listOut](t, call(t, session, ctx, "list_pages", map[string]any{"prefix": "guides/"}))
	if guides.Total == 0 {
		t.Fatal("prefix filter returned nothing")
	}
	for _, page := range guides.Pages {
		if !strings.HasPrefix(page.Path, "guides/") {
			t.Errorf("prefix filter let %q through", page.Path)
		}
	}
}

func TestResourcesServeSourceMarkdown(t *testing.T) {
	session, ctx := connect(t)

	list, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list.Resources) == 0 {
		t.Fatal("no resources advertised")
	}

	read, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "docs://guides/deploy.md"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(read.Contents))
	}
	if read.Contents[0].MIMEType != "text/markdown" {
		t.Errorf("MIMEType = %q, want text/markdown", read.Contents[0].MIMEType)
	}
	if !strings.Contains(read.Contents[0].Text, "# Deploying") {
		t.Error("the resource did not return the source Markdown")
	}
}

// recorder is an Observer that writes down what it hears, one line per event.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
}

func (r *recorder) Received(method string) { r.note("method %s", method) }
func (r *recorder) ToolCalled(tool, outcome string, _ time.Duration) {
	r.note("tool %s %s", tool, outcome)
}
func (r *recorder) PageRead(path, via string)     { r.note("read %s via %s", path, via) }
func (r *recorder) Searched(hits int, top string) { r.note("search %d %s", hits, top) }

func (r *recorder) saw(event string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == event {
			return true
		}
	}
	return false
}

func TestTheObserverHearsWhatAgentsAskFor(t *testing.T) {
	observer := &recorder{}
	session, ctx := connectWith(t, observer)

	call(t, session, ctx, "search_docs", map[string]any{"query": "rolling deployment"})
	call(t, session, ctx, "search_docs", map[string]any{"query": "zzzqqqxxx"})
	call(t, session, ctx, "get_page", map[string]any{"path": "guides/deploy"})
	call(t, session, ctx, "get_section", map[string]any{"path": "guides/deploy.md", "anchor": "rolling-updates"})
	call(t, session, ctx, "get_page", map[string]any{"path": "no/such/page.md"})
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "../../etc/passwd"}); err == nil {
		t.Fatal("an unknown tool was accepted")
	}
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "docs://index.md"}); err != nil {
		t.Fatalf("ReadResource: %v", err)
	}

	for _, want := range []string{
		"method tools/call",
		"method resources/read",
		"search 0 ",
		"search 1 guides/deploy.md",
		"read guides/deploy.md via get_page",
		"read guides/deploy.md via get_section",
		"read index.md via resource",
		"tool search_docs ok",
		"tool get_section ok",
		"tool get_page error", // no such page: the model is told how to recover
		"tool other failed",   // an unknown tool's name is not passed on
	} {
		if !observer.saw(want) {
			t.Errorf("the observer did not hear %q; it heard %q", want, observer.events)
		}
	}
	for _, e := range observer.events {
		if strings.Contains(e, "passwd") || strings.Contains(e, "zzzqqqxxx") {
			t.Errorf("something a client typed reached the observer: %q", e)
		}
	}
}

func TestObservedByReportsToAnotherObserver(t *testing.T) {
	project, err := mkdocs.Load("../../testdata/site")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	first, second := &recorder{}, &recorder{}
	service := New(project, Options{Observer: first})
	observed := service.ObservedBy(second)

	server := mcp.NewServer(&mcp.Implementation{Name: "mkdocsgo", Version: "test"}, nil)
	observed.Register(server)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	call(t, session, ctx, "get_page", map[string]any{"path": "index.md"})
	if !second.saw("read index.md via get_page") {
		t.Errorf("the new observer heard %q", second.events)
	}
	if len(first.events) != 0 {
		t.Errorf("the original observer heard %q; the copy must not report to it", first.events)
	}
}
