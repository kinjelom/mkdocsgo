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
	m := New("1.2.3", UsersNone, "")
	m.HTTPRequest("site", "internet", "", 200, 1500, 3*time.Millisecond)
	m.HTTPRequest("site", "internet", "", 200, 500, 3*time.Millisecond)
	m.HTTPRequest("mcp", "", "", 401, 48, time.Millisecond)

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
	m := New("test", UsersNone, "")
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
	text := render(t, New("test", UsersNone, ""))
	if strings.Contains(text, "mkdocsgo_mcp_searches_total") {
		t.Errorf("an empty family was rendered; some Pushgateway versions refuse those:\n%s", text)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	m := New("test", UsersNone, "")
	m.PageRead("odd \"name\"\\with\nnewline.md", "get_page")
	mustContain(t, render(t, m), `mkdocsgo_mcp_page_reads_total{page="odd \"name\"\\with\nnewline.md",via="get_page"} 1`)
}

func TestOnlyAServedPageCountsAsAView(t *testing.T) {
	m := New("test", UsersNone, "")
	m.SiteRequest("page", "/guides/", "", 200)
	m.SiteRequest("page", "/guides/", "", 304) // a revalidation is a reader too
	m.SiteRequest("page", "/guides/", "", 401) // asked for credentials, read nothing
	m.SiteRequest("not_found", "", "", 404)
	m.SiteRequest("stylesheet", "", "", 200)

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
	m := New("test", UsersNone, "")
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
	m.HTTPRequest("site", "", "", 200, 1, time.Millisecond)
	m.SiteRequest("page", "/", "", 200)
	m.Received("tools/list")
	m.ToolCalled("list_pages", "ok", time.Millisecond)
	m.PageRead("index.md", "resource")
	m.Searched(1, "index.md")
}

func TestTheHandlerServesTheTextFormat(t *testing.T) {
	m := New("test", UsersNone, "")
	m.Received("tools/list")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ContentType {
		t.Fatalf("status = %d, Content-Type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	mustContain(t, rec.Body.String(), `mkdocsgo_mcp_requests_total{method="tools/list"} 1`)
}

func TestUsersAreNotALabelUnlessAskedFor(t *testing.T) {
	m := New("test", UsersNone, "")
	if got := m.User("partner-a"); got != "" {
		t.Errorf("User() = %q with users off", got)
	}
	m.HTTPRequest("site", "internet", m.User("partner-a"), 200, 1, time.Millisecond)
	if text := render(t, m); strings.Contains(text, "user=") {
		t.Errorf("a user label appeared without -metrics-users:\n%s", text)
	}
}

func TestUsersByName(t *testing.T) {
	m := New("test", UsersName, "")
	user := m.User("partner-a")
	m.HTTPRequest("site", "internet", user, 200, 1, time.Millisecond)
	m.SiteRequest("page", "/guides/", user, 200)
	m.Caller(user).PageRead("guides/deploy.md", "get_section")
	m.Caller(user).ToolCalled("get_section", "ok", time.Millisecond)
	m.Caller(user).Searched(0, "")
	m.Caller(m.User("")).PageRead("index.md", "resource") // anonymous, in a public zone

	mustContain(t, render(t, m),
		`mkdocsgo_http_requests_total{route="site",zone="internet",code="200",user="partner-a"} 1`,
		`mkdocsgo_site_page_views_total{page="/guides/",user="partner-a"} 1`,
		`mkdocsgo_mcp_page_reads_total{page="guides/deploy.md",via="get_section",user="partner-a"} 1`,
		`mkdocsgo_mcp_tool_calls_total{tool="get_section",outcome="ok",user="partner-a"} 1`,
		`mkdocsgo_mcp_searches_total{result="empty",user="partner-a"} 1`,
		`mkdocsgo_mcp_page_reads_total{page="index.md",via="resource",user=""} 1`,
	)
}

func TestUsersByHash(t *testing.T) {
	plain := New("test", UsersHash, "").User("partner-a")
	salted := New("test", UsersHash, "a salt").User("partner-a")
	otherSalt := New("test", UsersHash, "another salt").User("partner-a")

	if len(plain) != 12 || len(salted) != 12 {
		t.Fatalf("hashes %q, %q: want 12 hex characters", plain, salted)
	}
	if plain == "partner-a" || strings.Contains(salted, "partner") {
		t.Error("the hash shows the name")
	}
	if salted == plain || salted == otherSalt {
		t.Error("the salt does not change the hash, so it keys nothing")
	}
	if again := New("test", UsersHash, "a salt").User("partner-a"); again != salted {
		t.Error("the same principal and salt gave two pseudonyms; a dashboard could not follow one person")
	}
	if New("test", UsersHash, "a salt").User("partner-b") == salted {
		t.Error("two principals share a pseudonym")
	}
}

func TestParseUsers(t *testing.T) {
	for value, want := range map[string]Users{"": UsersNone, "none": UsersNone, "name": UsersName, "hash": UsersHash} {
		if got, err := ParseUsers(value); err != nil || got != want {
			t.Errorf("ParseUsers(%q) = %v, %v", value, got, err)
		}
	}
	if _, err := ParseUsers("email"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
