// Command mkdocsgo serves a MkDocs project: the built site over HTTP, the
// documentation sources over the Model Context Protocol, or both from one
// process and one port.
//
// MkDocs itself is not here. Python builds the site at image build time; this
// binary only serves what came out, plus an index over the Markdown sources.
// The runtime has no Python, no plugins and no build step.
//
// Three modes:
//
//	site+mcp  the site at /, MCP at /mcp, liveness at /healthz   (default)
//	site      the site only - a drop-in replacement for an nginx container
//	mcp       MCP only - over stdio when -http is absent, else at /mcp
//
// Access is governed by zones, when the project has an mkdocsgo.yml: the
// address a request arrived at selects a policy, and a restricted zone asks a
// browser for HTTP Basic credentials and an agent for a bearer token - one it
// was given, or one it got by signing in through the built-in OAuth
// authorization server, which is how claude.ai connects. A project without
// that file serves everything publicly, as before. Origin
// validation on /mcp is unchanged and unrelated: that is DNS-rebinding
// defence, not access control.
//
// What is asked for can be counted for Prometheus: which pages people read,
// which tools agents call and which pages those return. -metrics-addr serves
// the counts on a listener of their own, -metrics-push sends them to a
// Pushgateway; with neither, nothing is counted.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/kinjelom/mkdocsgo/internal/authz"
	"github.com/kinjelom/mkdocsgo/internal/mcpserver"
	"github.com/kinjelom/mkdocsgo/internal/metrics"
	"github.com/kinjelom/mkdocsgo/internal/mkdocs"
	"github.com/kinjelom/mkdocsgo/internal/web"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	modeBoth = "site+mcp"
	modeSite = "site"
	modeMCP  = "mcp"
)

// metricsPushVariable is where -metrics-push is read from when the flag is
// not given. A gateway URL may carry a password, and an argument is visible to
// every user of the machine in the process list; the environment is not.
const metricsPushVariable = "MKDOCSGO_METRICS_PUSH_URL"

// metricsSaltVariable keys the hash -metrics-users hash labels with. Without
// it the hash is a plain SHA-256 of a principal's name, and the names are in
// mkdocsgo.yml for anyone who can read the repository.
const metricsSaltVariable = "MKDOCSGO_METRICS_USER_SALT"

type originList []string

func (o *originList) String() string { return strings.Join(*o, ",") }
func (o *originList) Set(v string) error {
	*o = append(*o, v)
	return nil
}

type config struct {
	mode        string
	project     string
	siteDir     string
	configPath  string
	httpAddr    string
	searchLimit int
	noResources bool
	noAccessLog bool
	origins     originList

	metricsAddr     string
	metricsPush     string
	metricsInterval time.Duration
	metricsJob      string
	metricsInstance string
	metricsUsers    string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.mode, "mode", modeBoth, "what to serve: site+mcp, site, or mcp")
	flag.StringVar(&cfg.project, "project", ".", "path to the MkDocs project (the directory holding mkdocs.yml)")
	flag.StringVar(&cfg.siteDir, "site-dir", "", "path to the built site; defaults to <project>/site")
	flag.StringVar(&cfg.configPath, "config", "", "path to the zone configuration; defaults to <project>/"+authz.DefaultFileName)
	flag.StringVar(&cfg.httpAddr, "http", defaultHTTPAddr(), "address to listen on, e.g. 0.0.0.0:8080; defaults to 0.0.0.0:$DOC_PORT or 0.0.0.0:$PORT; required except in mcp mode over stdio")
	flag.IntVar(&cfg.searchLimit, "search-limit", mcpserver.DefaultSearchLimit, "default number of search results")
	flag.BoolVar(&cfg.noResources, "no-resources", false, "expose MCP tools only, without one resource per page")
	flag.BoolVar(&cfg.noAccessLog, "no-access-log", false, "do not log HTTP requests")
	flag.Var(&cfg.origins, "allow-origin", "additional allowed Origin for /mcp; repeatable")
	flag.StringVar(&cfg.metricsAddr, "metrics-addr", "", "serve Prometheus metrics at /metrics on this separate address, e.g. 0.0.0.0:9090; off by default")
	flag.StringVar(&cfg.metricsPush, "metrics-push", "", "push metrics to this Pushgateway, e.g. http://pushgateway:9091; defaults to $"+metricsPushVariable+", off when neither is set")
	flag.DurationVar(&cfg.metricsInterval, "metrics-push-interval", 30*time.Second, "how often to push metrics")
	flag.StringVar(&cfg.metricsJob, "metrics-job", "mkdocsgo", "the job label pushed metrics are grouped under")
	flag.StringVar(&cfg.metricsInstance, "metrics-instance", "", "the instance label pushed metrics are grouped under; defaults to the host name")
	flag.StringVar(&cfg.metricsUsers, "metrics-users", "none", "label metrics with who asked: none, name (the principal) or hash (a pseudonym; salted by $"+metricsSaltVariable+")")
	showVersion := flag.Bool("version", false, "print the version and exit")
	newToken := flag.Bool("new-token", false, "mint a bearer token, print it and the line to paste into "+authz.DefaultFileName+", then quit")
	hashPassword := flag.Bool("hash-password", false, "hash a password for "+authz.DefaultFileName+", then quit")
	healthcheck := flag.String("healthcheck", "", "GET this URL, exit 0 if it answers 2xx, then quit; \"self\" means this server's own /healthz")
	mcpProbe := flag.String("mcp-probe", "", "ask this MCP endpoint for its tool list, exit 0 if it answers with tools, then quit")
	flag.Parse()
	if cfg.metricsPush == "" {
		cfg.metricsPush = strings.TrimSpace(os.Getenv(metricsPushVariable))
	}

	if *showVersion {
		fmt.Println("mkdocsgo", version)
		return
	}

	// Credential tools. They read nothing and serve nothing, so they run
	// before any project is loaded: `mkdocsgo -new-token` works in an empty
	// directory, which is where somebody preparing a configuration usually is.
	if *newToken {
		if err := printNewToken(); err != nil {
			fmt.Fprintln(os.Stderr, "could not mint a token:", err)
			os.Exit(1)
		}
		return
	}
	if *hashPassword {
		if err := printPasswordHash(); err != nil {
			fmt.Fprintln(os.Stderr, "could not hash the password:", err)
			os.Exit(1)
		}
		return
	}

	// The runtime image is distroless: no shell, no curl, no wget. The binary
	// probes itself so Docker HEALTHCHECK and the release smoke test have
	// something to call.
	if *healthcheck != "" {
		// Docker HEALTHCHECK cannot expand a variable in exec form, so "self"
		// resolves the port here instead of in a shell the image does not have.
		url := *healthcheck
		if url == "self" {
			addr := defaultHTTPAddr()
			if addr == "" {
				addr = "0.0.0.0:8080"
			}
			url = "http://127.0.0.1:" + addr[strings.LastIndex(addr, ":")+1:] + "/healthz"
		}
		if err := probe(url); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
			os.Exit(1)
		}
		return
	}

	if *mcpProbe != "" {
		tools, err := probeMCP(*mcpProbe)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp probe failed:", err)
			os.Exit(1)
		}
		fmt.Printf("%s answered with %d tools: %s\n", *mcpProbe, len(tools), strings.Join(tools, ", "))
		return
	}

	// stdout carries JSON-RPC in stdio mode, so every diagnostic goes to
	// stderr. One stray byte on stdout breaks the client handshake.
	logger := log.New(os.Stderr, "", log.LstdFlags)

	if err := run(cfg, logger); err != nil {
		logger.Fatalf("%v", err)
	}
}

func run(cfg config, logger *log.Logger) error {
	switch cfg.mode {
	case modeBoth, modeSite, modeMCP:
	default:
		return fmt.Errorf("unknown -mode %q: expected %s, %s or %s", cfg.mode, modeBoth, modeSite, modeMCP)
	}
	servesSite := cfg.mode == modeBoth || cfg.mode == modeSite
	servesMCP := cfg.mode == modeBoth || cfg.mode == modeMCP

	if servesSite && cfg.httpAddr == "" {
		return fmt.Errorf("-mode %s needs -http, for example -http 0.0.0.0:8080", cfg.mode)
	}

	// Loaded before the site and the index, because a configuration error
	// should cost a second rather than the time it takes to hash and compress
	// a whole site first.
	zones, err := loadZones(cfg)
	if err != nil {
		return err
	}
	// Only /mcp over HTTP can use the authorization server, so only then is
	// its key required: a site-only deployment or a stdio process with the
	// same mkdocsgo.yml has no use for it.
	if servesMCP && cfg.httpAddr != "" {
		if err := zones.UseOAuthKey(os.Getenv(authz.OAuthKeyVariable)); err != nil {
			return fmt.Errorf("cannot start the OAuth authorization server: %w", err)
		}
	}

	// Counting costs a map update per request; nobody pays it unless the
	// counts go somewhere. Where they go is checked here, before the site and
	// the index, for the same reason as the zones.
	var meter *metrics.Metrics
	var exports *metricsExports
	if cfg.metricsAddr != "" || cfg.metricsPush != "" {
		users, err := metrics.ParseUsers(cfg.metricsUsers)
		if err != nil {
			return err
		}
		salt := os.Getenv(metricsSaltVariable)
		meter = metrics.New(version, users, salt)
		if exports, err = prepareMetrics(cfg, meter, logger); err != nil {
			return err
		}
		switch {
		case users == metrics.UsersName:
			logger.Print("metrics: labelled with the name of the principal who asked")
		case users == metrics.UsersHash && salt != "":
			logger.Print("metrics: labelled with a salted hash of the principal who asked")
		case users == metrics.UsersHash:
			logger.Printf("metrics: labelled with an unsalted hash of the principal who asked - anyone with the names in %s can tell who is who; set $%s to prevent it",
				authz.DefaultFileName, metricsSaltVariable)
		}
	}

	var site *web.Site
	if servesSite {
		dir := cfg.siteDir
		if dir == "" {
			dir = cfg.project + "/site"
		}
		loaded, err := web.Load(dir)
		if err != nil {
			return fmt.Errorf("cannot load the built site: %w", err)
		}
		site = loaded
		raw, compressed := site.Bytes()
		logger.Printf("site: %d files, %s on disk, %s held gzipped (%s)",
			site.Len(), humanBytes(raw), humanBytes(compressed), dir)
	}

	// The Markdown is read once, for the MCP index and for the zones that
	// offer it on the site - which is why -mode site needs docs/ as soon as
	// one zone does.
	markdownZones := zones.MarkdownZones()
	offersMarkdown := site != nil && len(markdownZones) > 0
	var project *mkdocs.Project
	if servesMCP || offersMarkdown {
		loaded, err := mkdocs.Load(cfg.project)
		if err != nil {
			return fmt.Errorf("cannot load the MkDocs project: %w", err)
		}
		project = loaded
	}

	// With a built site at hand, it decides what is published - for the MCP
	// half as much as for the Markdown on the site. A page MkDocs did not
	// build, because a plugin excluded it or it is a draft, is not
	// documentation anyone was meant to read, whatever docs_dir holds.
	if site != nil && project != nil {
		paths := make([]string, 0, len(project.Pages))
		for _, page := range project.Pages {
			paths = append(paths, page.Path)
		}
		built := site.Built(paths)
		if dropped := project.Retain(func(path string) bool { return built[path] }); len(dropped) > 0 {
			logger.Printf("mkdocs: %d pages in docs_dir have no page in the built site and are left out: %s",
				len(dropped), listSome(dropped, 5))
		}
	}

	if offersMarkdown {
		sources := web.Sources{
			Pages: make(map[string][]byte, len(project.Pages)),
			// The same sections get_section returns, from the same code.
			Section: func(path, anchor string) (string, []string, bool) {
				page, ok := project.Page(path)
				if !ok {
					return "", nil, false
				}
				if section, ok := page.Section(anchor); ok {
					return section.Markdown(), nil, true
				}
				return "", page.Anchors(), false
			},
		}
		for _, page := range project.Pages {
			sources.Pages[page.Path] = []byte(page.Source)
		}
		offered := site.OfferSources(sources, zones.OffersMarkdown)
		logger.Printf("site: the Markdown of %d pages, offered in zone %s", offered, strings.Join(markdownZones, ", "))
	}

	var service *mcpserver.Service
	if servesMCP {
		options := mcpserver.Options{
			Version:      version,
			SearchLimit:  cfg.searchLimit,
			WithResource: !cfg.noResources,
		}
		if meter != nil {
			options.Observer = meter
		}
		service = mcpserver.New(project, options)
		logger.Printf("mcp: %q, %d pages, %d sections indexed (%s)",
			project.SiteName, service.Pages(), service.Sections(), project.ConfigAt)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if exports != nil {
		wait := exports.start(ctx, logger, meter, cfg.metricsInterval)
		// Cancelled before waiting, however run returns: the metrics listener
		// and the pusher only finish once the context is done, and the
		// pusher's last act - deleting its group - must happen before exit.
		defer func() {
			stop()
			wait()
		}()
	}

	// MCP only, no address: the transport is stdio, which is what a local
	// client launches. Nothing else can be served that way.
	if cfg.mode == modeMCP && cfg.httpAddr == "" {
		if zones != nil {
			// stdio has no Host to match a zone against, and the client is a
			// process the user started themselves. Saying so beats letting
			// someone believe a policy is in force here.
			logger.Print("auth: zones do not apply over stdio - the transport is a local pipe")
		}
		logger.Print("mcp: serving over stdio")
		err := newMCPServer(service).Run(ctx, &mcp.StdioTransport{})
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}

	return serveHTTP(ctx, logger, cfg, zones, site, service, meter)
}

// loadZones reads the zone configuration.
//
// A missing file at the default location is not an error: a project that never
// asked for zones serves everything publicly, exactly as it did before they
// existed. A missing file at a path someone passed explicitly is an error,
// because they said where it was.
func loadZones(cfg config) (*authz.Config, error) {
	path := cfg.configPath
	explicit := path != ""
	if !explicit {
		path = filepath.Join(cfg.project, authz.DefaultFileName)
	}
	zones, err := authz.Load(path)
	switch {
	case err == nil:
		return zones, nil
	case !explicit && errors.Is(err, fs.ErrNotExist):
		return nil, nil
	default:
		return nil, fmt.Errorf("cannot load the zone configuration: %w", err)
	}
}

// printNewToken implements -new-token.
func printNewToken() error {
	secret, hash, err := authz.NewToken()
	if err != nil {
		return err
	}
	// The secret is printed once and never stored anywhere: what goes into the
	// configuration is the digest below it.
	fmt.Printf("token (copy it now, it cannot be recovered):\n  %s\n\n", secret)
	fmt.Printf("paste into %s, under the principal it belongs to:\n", authz.DefaultFileName)
	fmt.Printf("    tokens:\n      - id: %s\n        hash: \"%s\"\n", time.Now().UTC().Format("2006-01"), hash)
	return nil
}

// printPasswordHash implements -hash-password.
func printPasswordHash() error {
	password, err := readPassword()
	if err != nil {
		return err
	}
	hash, err := authz.HashPassword(password)
	if err != nil {
		return err
	}
	fmt.Printf("\npaste into %s, under the principal it belongs to:\n", authz.DefaultFileName)
	fmt.Printf("    password: \"%s\"\n", hash)
	return nil
}

// readPassword takes the password from the terminal without echoing it, and
// from stdin when there is no terminal - which is how a script would call it.
func readPassword() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, "password: ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "again   : ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", fmt.Errorf("the two entries differ")
	}
	if len(first) == 0 {
		return "", fmt.Errorf("empty password")
	}
	return string(first), nil
}

func newMCPServer(service *mcpserver.Service) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mkdocsgo",
			Title:   service.SiteName() + " documentation",
			Version: version,
		},
		&mcp.ServerOptions{Instructions: service.Instructions()},
	)
	service.Register(server)
	return server
}

func serveHTTP(ctx context.Context, logger *log.Logger, cfg config, zones *authz.Config, site *web.Site, service *mcpserver.Service, meter *metrics.Metrics) error {
	mux := http.NewServeMux()

	// Outside every zone, deliberately. A platform health check arrives at the
	// container's address rather than at a route, so a zone would turn every
	// probe into a 403 and the application would never come up.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	if zones != nil {
		// Also outside every zone: a client reads this document in order to
		// learn how to authenticate, so requiring authentication for it would
		// be a closed loop.
		metadata := zones.Metadata()
		mux.Handle(authz.MetadataPath, metadata)
		mux.Handle(authz.MetadataPathMCP, metadata)
	}

	if service != nil {
		handler := mcp.NewStreamableHTTPHandler(
			func(r *http.Request) *mcp.Server {
				// Stateless, so this runs once per request - after the zone
				// check, whose identity is in the request's context. That is
				// how a tool call learns who made it, for the metrics' user
				// label.
				if meter.LabelsUsers() {
					return newMCPServer(service.ObservedBy(meter.Caller(meter.User(principalOf(r)))))
				}
				return newMCPServer(service)
			},
			// Stateless is the sessionless direction of the 2026-07-28 spec:
			// no Mcp-Session-Id, so any instance answers any request and the
			// platform needs no session affinity.
			&mcp.StreamableHTTPOptions{Stateless: true},
		)
		// An exact pattern beats the "/" subtree, so /mcp reaches MCP even
		// though the site is mounted at the root.
		//
		// The origin guard stays outside the zone check: a DNS-rebinding
		// attempt is rejected before it can make the process spend anything
		// on verifying a credential.
		mux.Handle("/mcp", guardOrigin(cfg.origins, zones.Middleware(authz.SurfaceMCP, handler)))

		if zones.OAuthEnabled() {
			// Outside every zone and outside the origin guard, like the
			// metadata: a client comes here because it has no credentials
			// yet, and the sign-in form is a page in the person's browser,
			// posting back to its own origin.
			oauth := zones.OAuthHandler()
			for _, path := range authz.OAuthPaths {
				mux.Handle(path, oauth)
			}
		}
	}
	if site != nil {
		mux.Handle("/", zones.Middleware(authz.SurfaceSite, site.Handler()))
	}

	var handler http.Handler = mux
	handler = recoverPanics(logger, handler)
	handler = observe(logger, !cfg.noAccessLog, meter, mux, site, handler)

	srv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: a streamed MCP response may legitimately stay open.
	}

	errc := make(chan error, 1)
	go func() {
		what := "site"
		switch {
		case site == nil:
			what = "mcp at /mcp"
		case service != nil:
			what = "site at / and mcp at /mcp"
		}
		logger.Printf("listening on http://%s - %s", cfg.httpAddr, what)
		logger.Printf("auth: %s", zones.Summary())
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// recoverPanics keeps one bad request from taking the process down.
//
// This matters more here than in a single-purpose server: the site and the MCP
// endpoint share a process, so a panic in a search would otherwise stop the
// documentation from being served too.
func recoverPanics(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, recovered)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush keeps streamed MCP responses working through the wrapper.
func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// observe writes the access log and counts the request, whichever of the two
// is on. Both want to know who made it and how it ended, so one wrapper finds
// out for both.
func observe(logger *log.Logger, accessLog bool, meter *metrics.Metrics, mux *http.ServeMux, site *web.Site, next http.Handler) http.Handler {
	if !accessLog && meter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		// The zone check runs underneath this handler and attaches its result
		// to a request this one never sees, so it is handed somewhere to put
		// it. Without a zone configuration nothing ever fills it in.
		ctx, identity := authz.WithRecorder(r.Context())
		next.ServeHTTP(rec, r.WithContext(ctx))
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		took := time.Since(started)
		who := identity.Identity()

		if accessLog {
			fields := who.LogFields()
			if fields != "" {
				fields = " " + fields
			}
			logger.Printf("%s %s %d %dB %s%s", r.Method, r.URL.Path, rec.status, rec.bytes,
				took.Round(time.Millisecond), fields)
		}

		if meter != nil {
			// The route is the pattern the mux chose, not the path, so a
			// scanner's thousand paths are one "site" and not a thousand
			// series.
			_, pattern := mux.Handler(r)
			route := routeOf(pattern)
			zone, principal := "", ""
			if who != nil {
				zone, principal = who.Zone, who.Principal
			}
			user := meter.User(principal)
			meter.HTTPRequest(route, zone, user, rec.status, int64(rec.bytes), took)
			if route == "site" && site != nil {
				kind, page := site.Describe(r.URL.Path)
				meter.SiteRequest(kind, page, user, rec.status)
			}
		}
	})
}

// listSome joins the first n items and says how many more there are, so one
// log line stays one line.
func listSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:n], ", "), len(items)-n)
}

// principalOf is who the zone check found behind a request, if anyone.
func principalOf(r *http.Request) string {
	if id := authz.IdentityFrom(r.Context()); id != nil {
		return id.Principal
	}
	return ""
}

// routeOf names the part of the server a mux pattern belongs to.
func routeOf(pattern string) string {
	switch {
	case pattern == "/":
		return "site"
	case pattern == "/mcp":
		return "mcp"
	case pattern == "/healthz":
		return "healthz"
	case strings.HasPrefix(pattern, "/.well-known/"), strings.HasPrefix(pattern, "/oauth/"):
		return "auth"
	}
	return "other"
}

// metricsExports is where the counts go: a listener of their own, a
// Pushgateway, or both.
//
// The listener is separate from -http on purpose. That one is the route the
// documentation is published on, often to the internet, and what people read
// and what agents ask is not something to publish with it.
type metricsExports struct {
	listener net.Listener
	pusher   *metrics.Pusher
}

// prepareMetrics checks the gateway address and binds the listener, so a bad
// URL or a port already taken fails the start instead of filling the log.
func prepareMetrics(cfg config, meter *metrics.Metrics, logger *log.Logger) (*metricsExports, error) {
	exports := &metricsExports{}
	if cfg.metricsPush != "" {
		instance := cfg.metricsInstance
		if instance == "" {
			instance, _ = os.Hostname()
		}
		pusher, err := metrics.NewPusher(meter, cfg.metricsPush, cfg.metricsJob, instance, cfg.metricsInterval, logger)
		if err != nil {
			return nil, fmt.Errorf("cannot push metrics: %w", err)
		}
		exports.pusher = pusher
	}
	if cfg.metricsAddr != "" {
		listener, err := net.Listen("tcp", cfg.metricsAddr)
		if err != nil {
			return nil, fmt.Errorf("cannot serve metrics: %w", err)
		}
		exports.listener = listener
	}
	return exports, nil
}

// start serves and pushes until ctx ends. The wait it returns blocks until
// both have finished.
func (e *metricsExports) start(ctx context.Context, logger *log.Logger, meter *metrics.Metrics, interval time.Duration) (wait func()) {
	var running sync.WaitGroup
	if e.listener != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", meter.Handler())
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		running.Add(2)
		go func() {
			defer running.Done()
			if err := srv.Serve(e.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Printf("metrics: %v", err)
			}
		}()
		go func() {
			defer running.Done()
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		logger.Printf("metrics: serving at http://%s/metrics", e.listener.Addr())
	}
	if e.pusher != nil {
		running.Add(1)
		go func() {
			defer running.Done()
			e.pusher.Run(ctx)
		}()
		logger.Printf("metrics: pushing to %s every %s", e.pusher.Target(), interval)
	}
	return running.Wait
}

// guardOrigin rejects cross-origin browser requests to the MCP endpoint.
//
// This is not authentication and does not pretend to be. It is the DNS
// rebinding defence the MCP spec requires of HTTP transports: without it a web
// page the user happens to visit could drive a server bound to their loopback.
// Requests with no Origin header - every non-browser MCP client - pass through.
func guardOrigin(allowed []string, next http.Handler) http.Handler {
	permitted := map[string]bool{}
	for _, origin := range allowed {
		permitted[strings.TrimRight(strings.TrimSpace(origin), "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin != "" && !permitted[origin] && !isLoopbackOrigin(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackOrigin(origin string) bool {
	for _, prefix := range []string{
		"http://localhost", "https://localhost",
		"http://127.0.0.1", "https://127.0.0.1",
		"http://[::1]", "https://[::1]",
	} {
		if origin == prefix || strings.HasPrefix(origin, prefix+":") {
			return true
		}
	}
	return false
}

// defaultHTTPAddr lets the container environment choose the port without a
// shell to expand it: DOC_PORT is what this project's nginx image used, PORT is
// what Cloud Foundry injects.
func defaultHTTPAddr() string {
	for _, name := range []string{"DOC_PORT", "PORT"} {
		if port := strings.TrimSpace(os.Getenv(name)); port != "" {
			return "0.0.0.0:" + port
		}
	}
	return ""
}

// probe implements -healthcheck.
func probe(url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("%s returned HTTP %d", url, response.StatusCode)
	}
	return nil
}

// probeMCP implements -mcp-probe.
//
// A GET against /mcp proves nothing: the endpoint answers POST. This sends a
// real tools/list call and checks that tools come back, which is what a client
// would need. The response may arrive as JSON or as a single SSE frame,
// depending on what the server negotiated, so both are accepted.
func probeMCP(url string) ([]string, error) {
	const request = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(request))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("%s returned HTTP %d", url, response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var payload struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	decoded := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &payload); err == nil {
			decoded = true
			break
		}
	}
	if !decoded {
		return nil, fmt.Errorf("could not parse a JSON-RPC response from %s", url)
	}
	if payload.Error != nil {
		return nil, fmt.Errorf("server replied with an error: %s", payload.Error.Message)
	}
	if len(payload.Result.Tools) == 0 {
		return nil, fmt.Errorf("%s advertised no tools", url)
	}

	names := make([]string, 0, len(payload.Result.Tools))
	for _, tool := range payload.Result.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
