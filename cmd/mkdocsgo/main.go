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
// browser for HTTP Basic credentials and an agent for a bearer token. A
// project without that file serves everything publicly, as before. Origin
// validation on /mcp is unchanged and unrelated: that is DNS-rebinding
// defence, not access control.
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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/kinjelom/mkdocsgo/internal/authz"
	"github.com/kinjelom/mkdocsgo/internal/mcpserver"
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
	showVersion := flag.Bool("version", false, "print the version and exit")
	newToken := flag.Bool("new-token", false, "mint a bearer token, print it and the line to paste into "+authz.DefaultFileName+", then quit")
	hashPassword := flag.Bool("hash-password", false, "hash a password for "+authz.DefaultFileName+", then quit")
	healthcheck := flag.String("healthcheck", "", "GET this URL, exit 0 if it answers 2xx, then quit; \"self\" means this server's own /healthz")
	mcpProbe := flag.String("mcp-probe", "", "ask this MCP endpoint for its tool list, exit 0 if it answers with tools, then quit")
	flag.Parse()

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

	var service *mcpserver.Service
	if servesMCP {
		project, err := mkdocs.Load(cfg.project)
		if err != nil {
			return fmt.Errorf("cannot load the MkDocs project: %w", err)
		}
		service = mcpserver.New(project, mcpserver.Options{
			Version:      version,
			SearchLimit:  cfg.searchLimit,
			WithResource: !cfg.noResources,
		})
		logger.Printf("mcp: %q, %d pages, %d sections indexed (%s)",
			project.SiteName, service.Pages(), service.Sections(), project.ConfigAt)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	return serveHTTP(ctx, logger, cfg, zones, site, service)
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

func serveHTTP(ctx context.Context, logger *log.Logger, cfg config, zones *authz.Config, site *web.Site, service *mcpserver.Service) error {
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
			func(*http.Request) *mcp.Server { return newMCPServer(service) },
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
	}
	if site != nil {
		mux.Handle("/", zones.Middleware(authz.SurfaceSite, site.Handler()))
	}

	var handler http.Handler = mux
	handler = recoverPanics(logger, handler)
	if !cfg.noAccessLog {
		handler = logRequests(logger, handler)
	}

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

func logRequests(logger *log.Logger, next http.Handler) http.Handler {
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
		who := identity.Identity().LogFields()
		if who != "" {
			who = " " + who
		}
		logger.Printf("%s %s %d %dB %s%s", r.Method, r.URL.Path, rec.status, rec.bytes,
			time.Since(started).Round(time.Millisecond), who)
	})
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
