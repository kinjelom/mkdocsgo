package metrics

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Pusher sends the metrics to a Pushgateway, or to anything that speaks its
// protocol - VictoriaMetrics 1.84 and later does, under
// /api/v1/import/prometheus - for
// platforms where nothing can scrape an instance directly. Cloud Foundry is
// the case in point: its router balances across instances, so a scrape
// through a route sees a different one each time.
//
// Each push replaces this instance's group - job and instance labels - with
// the current totals. On shutdown the group is deleted, so a replaced
// instance does not linger in the gateway as a flat line that looks alive.
type Pusher struct {
	metrics  *Metrics
	group    *url.URL
	interval time.Duration
	client   *http.Client
	logger   *log.Logger
}

// NewPusher checks the gateway address and works out the group's URL:
// base, then /metrics/job/<job>/instance/<instance>.
//
// Credentials may be written into base as user:password@, and are sent as
// HTTP Basic. A caller should take base from the environment rather than a
// command-line flag when it carries them: an argument is visible to every
// user of the machine in the process list.
func NewPusher(m *Metrics, base, job, instance string, interval time.Duration, logger *log.Logger) (*Pusher, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("push address %q is not an http or https URL", redact(base))
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("push address %s: a query or fragment has no place in a Pushgateway URL", u.Redacted())
	}
	if job == "" {
		return nil, fmt.Errorf("the job label cannot be empty")
	}
	if interval < time.Second {
		return nil, fmt.Errorf("a push interval of %s would hammer the gateway; use a second or more", interval)
	}
	// Built escaped and unescaped side by side rather than resolved as a
	// reference, which would fold a label value such as ".." into the path.
	escaped := strings.TrimRight(u.EscapedPath(), "/") + "/metrics" +
		groupingSegment("job", job) + groupingSegment("instance", instance)
	if u.Path, err = url.PathUnescape(escaped); err != nil {
		return nil, err
	}
	u.RawPath = escaped
	return &Pusher{
		metrics:  m,
		group:    u,
		interval: interval,
		client:   &http.Client{Timeout: 10 * time.Second},
		logger:   logger,
	}, nil
}

// Target is where the pushes go, without credentials, for the startup log.
func (p *Pusher) Target() string { return p.group.Redacted() }

// Run pushes at once, then every interval, until ctx is cancelled; then it
// deletes the group. Failures are logged when they start and when they stop,
// not every interval: a gateway that is down for an hour is one line, not
// a hundred and twenty.
func (p *Pusher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	failing := false
	for {
		err := p.push(ctx)
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			p.logger.Printf("metrics: push to %s failed: %v - retrying every %s", p.Target(), err, p.interval)
			failing = true
		case err == nil && failing:
			p.logger.Printf("metrics: push to %s works again", p.Target())
			failing = false
		}
		select {
		case <-ctx.Done():
			// The context that ended the loop cannot carry the request that
			// tidies up after it.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.send(cleanup, http.MethodDelete, nil); err != nil {
				p.logger.Printf("metrics: could not delete %s from the gateway: %v", p.Target(), err)
			}
			return
		case <-ticker.C:
		}
	}
}

// push sends the current totals. POST replaces every metric family it
// carries, and it always carries all of them, so the group ends up exactly
// as this process sees it - and POST is what VictoriaMetrics accepts too.
func (p *Pusher) push(ctx context.Context) error {
	var body bytes.Buffer
	if err := p.metrics.registry.writeText(&body); err != nil {
		return err
	}
	return p.send(ctx, http.MethodPost, &body)
}

func (p *Pusher) send(ctx context.Context, method string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, method, p.group.String(), body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", ContentType)
	}
	res, err := p.client.Do(req)
	if err != nil {
		// net/http strips the password from the URL in its errors.
		return err
	}
	defer res.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		if text := strings.TrimSpace(string(detail)); text != "" {
			return fmt.Errorf("HTTP %d: %s", res.StatusCode, text)
		}
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

// groupingSegment is one label of the Pushgateway grouping key as a path
// segment. A value that is empty, contains a slash or is a dot segment cannot
// be a path segment as it is, so it goes base64url-encoded under name@base64,
// which the Pushgateway defines for exactly this.
func groupingSegment(name, value string) string {
	if value == "" {
		return "/" + name + "@base64/="
	}
	if strings.Contains(value, "/") || value == "." || value == ".." {
		return "/" + name + "@base64/" + base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	return "/" + name + "/" + url.PathEscape(value)
}

func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Redacted()
	}
	return "(unparseable)"
}
