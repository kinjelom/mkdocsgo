# Metrics

What the server is asked for, counted for Prometheus: which pages people read
on the site, which tools agents call, which pages those tools hand back, and
how many questions found nothing. Scraped from a listener of its own, or pushed
to a Pushgateway.

**Off by default.** `-metrics-addr` or `-metrics-push` turns it on; with
neither, nothing is counted and nothing is paid for.

## Two ways out

|                  | Scrape                                                | Push                                                                     |
|------------------|-------------------------------------------------------|--------------------------------------------------------------------------|
| Turned on by     | `-metrics-addr 0.0.0.0:9090`                          | `-metrics-push http://pushgateway:9091`, or `$MKDOCSGO_METRICS_PUSH_URL` |
| Prometheus reads | each instance, at `/metrics`                          | the Pushgateway, which holds each instance's last push                   |
| Right for        | Kubernetes, Docker, a VM - an instance can be reached | Cloud Foundry, or anywhere an instance cannot be reached directly        |

Both can be on at once.

### Scraping

`-metrics-addr` starts a second listener that serves `/metrics` and nothing
else. It is never on the `-http` address: that one is the route the
documentation is published on, often to the internet, and what people read and
what agents ask is not something to publish with it. On `-http`, `/metrics` is
simply a page the site does not have.

```yaml
# Kubernetes: a second container port, scraped per pod
          args: ["-mode", "site+mcp", "-project", "/project", "-http", "0.0.0.0:8080",
                 "-metrics-addr", "0.0.0.0:9090"]
          ports:
            - {name: http,    containerPort: 8080}
            - {name: metrics, containerPort: 9090}
```

A `PodMonitor` pointing at the `metrics` port, or the `prometheus.io/scrape`
annotations your Prometheus honours, does the rest. Scrape the pods, not the
Service: a Service balances across them, and each scrape would see a
different instance's counters.

### Pushing

`-metrics-push` sends everything to a Pushgateway every
`-metrics-push-interval` (30 s), as a `POST` of the text format to

```
<url>/metrics/job/<job>/instance/<instance>
```

| Flag                     | Default       | Meaning                                                        |
|--------------------------|---------------|----------------------------------------------------------------|
| `-metrics-push`          | -             | The gateway; falls back to `$MKDOCSGO_METRICS_PUSH_URL`        |
| `-metrics-push-interval` | `30s`         | How often                                                      |
| `-metrics-job`           | `mkdocsgo`    | The `job` label. Give each application its own                 |
| `-metrics-instance`      | the host name | The `instance` label. Must differ between instances of one job |

On a clean shutdown the group is **deleted**, so an instance that is gone does
not stay in the gateway as a flat line that looks alive. A value with a `/` in
it, or an empty one, is sent base64-encoded as the Pushgateway defines.

**Credentials** go into the URL - `https://user:password@gateway.example.com` -
and are sent as HTTP Basic. Put that URL in `MKDOCSGO_METRICS_PUSH_URL`, not
on the command line: an argument is visible to every user of the machine in the
process list. The startup log prints the URL with the password masked.

**VictoriaMetrics** (1.84 or newer) accepts the same protocol, without a
Pushgateway in between: `-metrics-push http://victoria:8428/api/v1/import/prometheus`.
If it refuses the `DELETE` at shutdown, the log says so once, and it costs
nothing: VictoriaMetrics stores samples, not a last value that could go stale.

Two things the Pushgateway asks of Prometheus and of you:

- scrape it with `honor_labels: true`, or every series gets the gateway's own
  `job` and `instance` instead of the ones this server pushed;
- an instance that crashed never deleted its group. `push_time_seconds` says
  when each group last heard from its instance, so
  `time() - push_time_seconds{job="mkdocsgo"} > 120` finds the dead ones.

#### On Cloud Foundry

The host name inside a CF container is the instance's GUID, which is unique -
what the `instance` label needs - and new after every restart, which is why the
delete on shutdown matters. With the binary buildpack the start command goes
through a shell, so a stable label is one variable away:

```yaml
    command: ./mkdocsgo -mode site+mcp -project . -metrics-job docs-example -metrics-instance "$CF_INSTANCE_INDEX"
```

The gateway URL goes in with `cf set-env`, as the OAuth key does:

```bash
cf set-env mkdocsgo-example MKDOCSGO_METRICS_PUSH_URL "https://user:password@pushgateway.example.com"
cf restart mkdocsgo-example
```

## What is counted

### The site

| Metric                           | Labels         | Counts                                                                                                                  |
|----------------------------------|----------------|-------------------------------------------------------------------------------------------------------------------------|
| `mkdocsgo_site_page_views_total` | `page`         | A page served to a reader: `200`, `206`, or a `304` revalidation. `page` is its URL - `/guides/` however it was spelled |
| `mkdocsgo_site_requests_total`   | `kind`, `code` | Every site request, by what it asked for and how it ended                                                               |

`kind` is `page`, `stylesheet`, `script`, `image`, `font`, `search_index`,
`other` or `not_found`. `search_index` is Material's `search_index.json`, the
index a browser downloads to search on its own. What readers type into the
search box never reaches the server, so it cannot be counted here.

A `401` from a restricted zone is a request for a page, not a view of it.

### MCP

| Metric                               | Labels            | Counts                                                                                                            |
|--------------------------------------|-------------------|-------------------------------------------------------------------------------------------------------------------|
| `mkdocsgo_mcp_requests_total`        | `method`          | JSON-RPC methods: `tools/call`, `tools/list`, `resources/read`, `server/discover`, ...                            |
| `mkdocsgo_mcp_tool_calls_total`      | `tool`, `outcome` | Tool calls. `ok`; `error` - a tool error the model can correct, such as no such page; `failed` - a protocol error |
| `mkdocsgo_mcp_tool_duration_seconds` | `tool`            | Histogram of the time spent inside the server                                                                     |
| `mkdocsgo_mcp_page_reads_total`      | `page`, `via`     | Pages handed to an agent, by source path, and how: `get_page`, `get_section`, `resource`                          |
| `mkdocsgo_mcp_searches_total`        | `result`          | `search_docs` calls: `hits`, or `empty` - a question the documentation did not answer                             |
| `mkdocsgo_mcp_search_top_hits_total` | `page`            | How often each page was the best match of a search                                                                |

`page` here is the source path under `docs_dir` - `guides/deploy.md` - because
that is what an agent reads; the site's `page` is a URL, because that is what
a browser does.

### HTTP and the process

| Metric                                                                   | Labels                  | Counts                                                                |
|--------------------------------------------------------------------------|-------------------------|-----------------------------------------------------------------------|
| `mkdocsgo_http_requests_total`                                           | `route`, `zone`, `code` | Every request. `route` is `site`, `mcp`, `auth`, `healthz` or `other` |
| `mkdocsgo_http_request_duration_seconds`                                 | `route`                 | Histogram, from the request to the end of the response                |
| `mkdocsgo_http_response_bytes_total`                                     | `route`                 | Bytes written; compressed bytes where the response was compressed     |
| `mkdocsgo_build_info`                                                    | `version`, `goversion`  | Always 1                                                              |
| `process_start_time_seconds`                                             | -                       | When the process started                                              |
| `go_goroutines`, `go_memstats_heap_alloc_bytes`, `go_memstats_sys_bytes` | -                       | The Go runtime                                                        |

`zone` is the zone from [AUTH.md](./AUTH.md), empty without an `mkdocsgo.yml`;
`auth` is the resource metadata and the OAuth endpoints.

## What is never a label

**Anything a client typed.** Not a request path - a page that is not in the
build is `not_found`, whatever was asked for - not a search query, not a tool
name the server does not have (`other`), not a JSON-RPC method the SDK does not
know (it is refused before it is counted). A metric labelled by client input is
a denial of service against whatever stores it: one scanner walking
`/wp-admin` variants would mint a series per path.

**Who.** The zone is a label, the principal is not. The access log names the
principal of every request; a time series per person is a different kind of
record, and not one to start keeping as a side effect.

**The question.** An empty search is counted; what it was is not stored
anywhere. `result="empty"` rising says the documentation has a gap, not which.

So the number of series is bounded by the build and the configuration: pages
times `via`, routes times zones times status codes, kinds times status codes.
For a site of a hundred pages that is a few hundred series.

## Queries worth having

```promql
# What people read, this week
topk(10, sum by (page) (increase(mkdocsgo_site_page_views_total[7d])))

# What agents read, this week
topk(10, sum by (page) (increase(mkdocsgo_mcp_page_reads_total[7d])))

# The share of agent questions the documentation did not answer
sum(increase(mkdocsgo_mcp_searches_total{result="empty"}[1d]))
  / sum(increase(mkdocsgo_mcp_searches_total[1d]))

# Tool calls the model got wrong - a bad path, a stale anchor
sum by (tool) (increase(mkdocsgo_mcp_tool_calls_total{outcome!="ok"}[1d]))

# Refused credentials, by zone
sum by (zone) (rate(mkdocsgo_http_requests_total{code="401"}[5m]))

# 95th percentile latency, by route
histogram_quantile(0.95, sum by (le, route) (rate(mkdocsgo_http_request_duration_seconds_bucket[5m])))
```

Sum across instances; each one counts only what it answered.

## Why it is written by hand

The metrics are counters, histograms and a few gauges, each with its label
names fixed in advance. The text format for those is a page of code, and the
Pushgateway protocol is an HTTP `POST` of the same text. `client_golang` would
bring a protobuf runtime and half a dozen modules into a binary that keeps its
dependency list short on purpose - the same reason the search index is
hand-rolled.

## Limits

- **Counters start at zero with the process.** Prometheus's `rate` and
  `increase` handle a reset; a raw counter value does not mean "since the
  deployment".
- **No remote write and no OTLP.** Pushing speaks the Pushgateway protocol,
  which the Pushgateway and VictoriaMetrics accept.
- **A crashed instance leaves its group in the Pushgateway** until someone
  deletes it. See `push_time_seconds` above.
- **stdio counts too.** With `-mode mcp` and no `-http`, the MCP metrics are
  kept, and `-metrics-addr` and `-metrics-push` work as they do anywhere else.
