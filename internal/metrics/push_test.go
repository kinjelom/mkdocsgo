package metrics

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateway records what a Pushgateway would have been sent.
type gateway struct {
	mu       sync.Mutex
	requests []received
	status   int
}

type received struct {
	method, path, user, password, contentType, body string
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	user, password, _ := r.BasicAuth()
	g.mu.Lock()
	g.requests = append(g.requests, received{r.Method, r.URL.EscapedPath(), user, password, r.Header.Get("Content-Type"), string(body)})
	status := g.status
	g.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (g *gateway) seen() []received {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]received(nil), g.requests...)
}

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func TestAPushGoesToTheGroupAndIsDeletedOnShutdown(t *testing.T) {
	gw := &gateway{}
	server := httptest.NewServer(gw)
	defer server.Close()

	m := New("test", UsersNone, "")
	m.Received("tools/call")
	base := strings.Replace(server.URL, "http://", "http://pusher:s3cret@", 1) + "/prefix/"
	pusher, err := NewPusher(m, base, "mkdocsgo", "docs-0", time.Hour, quiet())
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}
	if strings.Contains(pusher.Target(), "s3cret") {
		t.Errorf("Target() = %q shows the password; it goes into the startup log", pusher.Target())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pusher.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(gw.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	requests := gw.seen()
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want a push and a delete: %+v", len(requests), requests)
	}
	push, remove := requests[0], requests[1]
	const group = "/prefix/metrics/job/mkdocsgo/instance/docs-0"
	if push.method != http.MethodPost || push.path != group {
		t.Errorf("push: %s %s, want POST %s", push.method, push.path, group)
	}
	if push.user != "pusher" || push.password != "s3cret" {
		t.Errorf("push credentials = %q:%q, want the ones written into the URL", push.user, push.password)
	}
	if push.contentType != ContentType || !strings.Contains(push.body, `mkdocsgo_mcp_requests_total{method="tools/call"} 1`) {
		t.Errorf("push body (%s):\n%s", push.contentType, push.body)
	}
	if remove.method != http.MethodDelete || remove.path != group {
		t.Errorf("on shutdown: %s %s, want DELETE %s", remove.method, remove.path, group)
	}
}

func TestAGroupingValueThatCannotBeAPathSegmentIsEncoded(t *testing.T) {
	for value, want := range map[string]string{
		"docs-0":        "/instance/docs-0",
		"apps/docs/0":   "/instance@base64/YXBwcy9kb2NzLzA",
		"":              "/instance@base64/=",
		"..":            "/instance@base64/Li4",
		"with space":    "/instance/with%20space",
		"żółć-instance": "/instance/%C5%BC%C3%B3%C5%82%C4%87-instance",
	} {
		if got := groupingSegment("instance", value); got != want {
			t.Errorf("groupingSegment(%q) = %q, want %q", value, got, want)
		}
	}

	pusher, err := NewPusher(New("test", UsersNone, ""), "http://gw:9091", "mkdocsgo", "..", time.Minute, quiet())
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}
	if got := pusher.Target(); got != "http://gw:9091/metrics/job/mkdocsgo/instance@base64/Li4" {
		t.Errorf("Target() = %q - a dot segment must not fold the path", got)
	}
}

func TestABadPushAddressFailsAtStartup(t *testing.T) {
	for _, base := range []string{"pushgateway:9091", "ftp://gw", "http://gw:9091/?x=1", "http://user:pw@"} {
		_, err := NewPusher(New("test", UsersNone, ""), base, "mkdocsgo", "a", time.Minute, quiet())
		if err == nil {
			t.Errorf("%q was accepted", base)
			continue
		}
		if strings.Contains(err.Error(), "pw") {
			t.Errorf("the error shows the password: %v", err)
		}
	}
	if _, err := NewPusher(New("test", UsersNone, ""), "http://gw", "mkdocsgo", "a", time.Millisecond, quiet()); err == nil {
		t.Error("a one-millisecond interval was accepted")
	}
}

func TestAFailingGatewayIsReportedOnceAndItsRecoveryToo(t *testing.T) {
	gw := &gateway{status: http.StatusBadRequest}
	server := httptest.NewServer(gw)
	defer server.Close()

	var logged strings.Builder
	var mu sync.Mutex
	logger := log.New(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}), "", 0)

	pusher, err := NewPusher(New("test", UsersNone, ""), server.URL, "mkdocsgo", "a", time.Second, logger)
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}
	pusher.interval = 20 * time.Millisecond // below what NewPusher allows, for the test's sake

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pusher.Run(ctx)
		close(done)
	}()
	for len(gw.seen()) < 4 {
		time.Sleep(5 * time.Millisecond)
	}
	gw.mu.Lock()
	gw.status = http.StatusOK
	gw.mu.Unlock()
	before := len(gw.seen())
	for len(gw.seen()) < before+2 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	text := logged.String()
	if strings.Count(text, "failed") != 1 || strings.Count(text, "works again") != 1 {
		t.Errorf("log:\n%s\nwant one failure and one recovery, however many pushes failed", text)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
