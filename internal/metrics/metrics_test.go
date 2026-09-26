package metrics

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, m *Metrics) string {
	t.Helper()
	var out bytes.Buffer
	if err := m.registry.writeText(&out); err != nil {
		t.Fatalf("writeText: %v", err)
	}
	return out.String()
}

func mustContain(t *testing.T, text string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("missing line %q in:\n%s", line, text)
		}
	}
}

func TestCountersRenderInTheTextFormat(t *testing.T) {
	m := New("1.2.3")
	m.HTTPRequest("site", "internet", 200, 1500, 3*time.Millisecond)
	m.HTTPRequest("site", "internet", 200, 500, 3*time.Millisecond)
	m.HTTPRequest("mcp", "", 401, 48, time.Millisecond)

	text := render(t, m)
	mustContain(t, text,
		"# TYPE mkdocsgo_http_requests_total counter",
		`mkdocsgo_http_requests_total{route="mcp",zone="",code="401"} 1`,
		`mkdocsgo_http_requests_total{route="site",zone="internet",code="200"} 2`,
		`mkdocsgo_http_response_bytes_total{route="site"} 2000`,
		`mkdocsgo_build_info{version="1.2.3",goversion="`+runtime.Version()+`"} 1`,
		"# TYPE go_goroutines gauge",
	)
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	m := New("test")
	m.ToolCalled("search_docs", "ok", 2*time.Millisecond)
	m.ToolCalled("search_docs", "ok", 20*time.Millisecond)
	m.ToolCalled("search_docs", "ok", 20*time.Second)

	text := render(t, m)
	mustContain(t, text,
		"# TYPE mkdocsgo_mcp_tool_duration_seconds histogram",
		`mkdocsgo_mcp_tool_duration_seconds_bucket{tool="search_docs",le="0.001"} 0`,
		`mkdocsgo_mcp_tool_duration_seconds_bucket{tool="search_docs",le="0.0025"} 1`,
		`mkdocsgo_mcp_tool_duration_seconds_bucket{tool="search_docs",le="0.025"} 2`,
		`mkdocsgo_mcp_tool_duration_seconds_bucket{tool="search_docs",le="10"} 2`,
		`mkdocsgo_mcp_tool_duration_seconds_bucket{tool="search_docs",le="+Inf"} 3`,
		`mkdocsgo_mcp_tool_duration_seconds_count{tool="search_docs"} 3`,
		`mkdocsgo_mcp_tool_calls_total{tool="search_docs",outcome="ok"} 3`,
	)
}

func TestAFamilyWithNothingCountedIsLeftOut(t *testing.T) {
	text := render(t, New("test"))
	if strings.Contains(text, "mkdocsgo_mcp_searches_total") {
		t.Errorf("an empty family was rendered; some Pushgateway versions refuse those:\n%s", text)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	m := New("test")
	m.PageRead("odd \"name\"\\with\nnewline.md", "get_page")
	mustContain(t, render(t, m), `mkdocsgo_mcp_page_reads_total{page="odd \"name\"\\with\nnewline.md",via="get_page"} 1`)
}

func TestOnlyAServedPageCountsAsAView(t *testing.T) {
	m := New("test")
	m.SiteRequest("page", "/guides/", 200)
	m.SiteRequest("page", "/guides/", 304) // a revalidation is a reader too
	m.SiteRequest("page", "/guides/", 401) // asked for credentials, read nothing
	m.SiteRequest("not_found", "", 404)
	m.SiteRequest("stylesheet", "", 200)

	text := render(t, m)
	mustContain(t, text,
		`mkdocsgo_site_page_views_total{page="/guides/"} 2`,
		`mkdocsgo_site_requests_total{kind="page",code="401"} 1`,
		`mkdocsgo_site_requests_total{kind="not_found",code="404"} 1`,
		`mkdocsgo_site_requests_total{kind="stylesheet",code="200"} 1`,
	)
	if strings.Contains(text, `page_views_total{page=""}`) {
		t.Error("a request with no page was counted as a view")
	}
}

func TestSearchesCountTheEmptyOnesAndTheBestPage(t *testing.T) {
	m := New("test")
	m.Searched(3, "guides/deploy.md")
	m.Searched(0, "")
	mustContain(t, render(t, m),
		`mkdocsgo_mcp_searches_total{result="empty"} 1`,
		`mkdocsgo_mcp_searches_total{result="hits"} 1`,
		`mkdocsgo_mcp_search_top_hits_total{page="guides/deploy.md"} 1`,
	)
}

func TestANilMetricsCountsNothingAndDoesNotPanic(t *testing.T) {
	var m *Metrics
	m.HTTPRequest("site", "", 200, 1, time.Millisecond)
	m.SiteRequest("page", "/", 200)
	m.Received("tools/list")
	m.ToolCalled("list_pages", "ok", time.Millisecond)
	m.PageRead("index.md", "resource")
	m.Searched(1, "index.md")
}

func TestTheHandlerServesTheTextFormat(t *testing.T) {
	m := New("test")
	m.Received("tools/list")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ContentType {
		t.Fatalf("status = %d, Content-Type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	mustContain(t, rec.Body.String(), `mkdocsgo_mcp_requests_total{method="tools/list"} 1`)
}
