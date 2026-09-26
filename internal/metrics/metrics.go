// Package metrics counts what the server is asked for - which pages people
// read on the site, which tools agents call and which pages those tools
// return - and renders the counts in the Prometheus text exposition format:
// for a scrape at /metrics, or for a push to a Pushgateway.
//
// It is written by hand rather than with client_golang. What is needed is
// counters, histograms and a few gauges, every one with its label names fixed
// in advance, and the format for those is a page of code. The client library
// would bring a protobuf runtime and half a dozen modules into a binary that
// has kept its dependency list short on purpose.
//
// Every label value comes from a closed set: a route, a zone from the
// configuration, a tool this server registered, a page that exists in the
// build. Never a raw request path and never a search query - a metric
// labelled by what a client typed is a denial of service against whatever
// stores it, and one scanner walking /wp-admin variants would be enough.
package metrics

import (
	"bufio"
	"net/http"
	"runtime"
	"strconv"
	"time"
)

// durationBuckets suit both a request and a tool call: a cached page is
// answered in well under a millisecond, a search in a few, and anything past
// a second is the thing worth seeing.
var durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Metrics is every metric the server keeps. A nil *Metrics is valid and
// counts nothing, which is what a server started without -metrics-addr or
// -metrics-push has.
type Metrics struct {
	registry registry

	httpRequests *counterVec
	httpDuration *histogramVec
	httpBytes    *counterVec

	siteRequests *counterVec
	pageViews    *counterVec

	mcpRequests  *counterVec
	toolCalls    *counterVec
	toolDuration *histogramVec
	pageReads    *counterVec
	searches     *counterVec
	topHits      *counterVec
}

// New declares every metric. version is reported in mkdocsgo_build_info.
func New(version string) *Metrics {
	m := &Metrics{}
	r := &m.registry

	info := newGaugeFunc(r, "mkdocsgo_build_info", "The running version, as labels; always 1.", func() float64 { return 1 })
	info.labels = []string{"version", "goversion"}
	info.values = []string{version, runtime.Version()}
	started := float64(time.Now().Unix())
	newGaugeFunc(r, "process_start_time_seconds", "When the process started, in seconds since the Unix epoch.", func() float64 { return started })
	r.add(runtimeFamily{})

	m.httpRequests = newCounterVec(r, "mkdocsgo_http_requests_total",
		"HTTP requests answered, by route (site, mcp, auth, healthz, other), zone and status code.",
		"route", "zone", "code")
	m.httpDuration = newHistogramVec(r, "mkdocsgo_http_request_duration_seconds",
		"Time from receiving a request to finishing the response, by route.",
		durationBuckets, "route")
	m.httpBytes = newCounterVec(r, "mkdocsgo_http_response_bytes_total",
		"Response body bytes written, by route; compressed bytes where the response was compressed.",
		"route")

	m.siteRequests = newCounterVec(r, "mkdocsgo_site_requests_total",
		"Requests to the site, by what was asked for (page, stylesheet, script, image, font, search_index, other, not_found) and status code.",
		"kind", "code")
	m.pageViews = newCounterVec(r, "mkdocsgo_site_page_views_total",
		"Pages of the site served to a reader - 200, 206 or a 304 revalidation - by URL path. Only pages that exist in the build appear.",
		"page")

	m.mcpRequests = newCounterVec(r, "mkdocsgo_mcp_requests_total",
		"MCP requests and notifications received, by JSON-RPC method.",
		"method")
	m.toolCalls = newCounterVec(r, "mkdocsgo_mcp_tool_calls_total",
		"MCP tool calls, by tool and outcome: ok, error (a tool error the model can correct, such as no such page) or failed (a protocol error).",
		"tool", "outcome")
	m.toolDuration = newHistogramVec(r, "mkdocsgo_mcp_tool_duration_seconds",
		"Time a tool call took inside the server, by tool.",
		durationBuckets, "tool")
	m.pageReads = newCounterVec(r, "mkdocsgo_mcp_page_reads_total",
		"Documentation pages agents read, by source path and how: get_page, get_section or resource.",
		"page", "via")
	m.searches = newCounterVec(r, "mkdocsgo_mcp_searches_total",
		"search_docs calls, by whether anything matched: hits or empty. An empty search is a question the documentation did not answer.",
		"result")
	m.topHits = newCounterVec(r, "mkdocsgo_mcp_search_top_hits_total",
		"How often each page was the best match of a search, by source path.",
		"page")
	return m
}

// Handler serves the metrics for a scrape.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", ContentType)
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		_ = m.registry.writeText(w)
	})
}

// HTTPRequest counts one answered HTTP request.
func (m *Metrics) HTTPRequest(route, zone string, status int, bytes int64, took time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.inc(route, zone, strconv.Itoa(status))
	m.httpDuration.observe(took.Seconds(), route)
	m.httpBytes.add(float64(bytes), route)
}

// SiteRequest counts one request to the site. kind and page are what the site
// says the path is, never the path itself; a page counts as viewed when it was
// served, in whole or in part, or revalidated.
func (m *Metrics) SiteRequest(kind, page string, status int) {
	if m == nil {
		return
	}
	m.siteRequests.inc(kind, strconv.Itoa(status))
	if page == "" {
		return
	}
	switch status {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified:
		m.pageViews.inc(page)
	}
}

// Received counts an MCP method.
func (m *Metrics) Received(method string) {
	if m == nil {
		return
	}
	m.mcpRequests.inc(method)
}

// ToolCalled counts a tool call and how long it took.
func (m *Metrics) ToolCalled(tool, outcome string, took time.Duration) {
	if m == nil {
		return
	}
	m.toolCalls.inc(tool, outcome)
	m.toolDuration.observe(took.Seconds(), tool)
}

// PageRead counts a documentation page handed to an agent.
func (m *Metrics) PageRead(page, via string) {
	if m == nil {
		return
	}
	m.pageReads.inc(page, via)
}

// Searched counts a search, and the page that answered it best, if any did.
func (m *Metrics) Searched(hits int, top string) {
	if m == nil {
		return
	}
	if hits == 0 {
		m.searches.inc("empty")
		return
	}
	m.searches.inc("hits")
	if top != "" {
		m.topHits.inc(top)
	}
}

// runtimeFamily is the Go runtime's view of the process, read once per render
// so that the two memory figures come from the same moment.
type runtimeFamily struct{}

func (runtimeFamily) write(w *bufio.Writer) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	for _, g := range []struct {
		name, help string
		value      float64
	}{
		{"go_goroutines", "Goroutines that currently exist.", float64(runtime.NumGoroutine())},
		{"go_memstats_heap_alloc_bytes", "Bytes of allocated heap objects.", float64(stats.HeapAlloc)},
		{"go_memstats_sys_bytes", "Bytes of memory obtained from the operating system.", float64(stats.Sys)},
	} {
		writeHeader(w, g.name, g.help, "gauge")
		writeSample(w, g.name, nil, nil, "", "", g.value)
	}
}
