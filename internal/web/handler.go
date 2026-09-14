package web

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"strings"
)

// SecurityHeaders are set on every response, success or error.
//
// They are declared here, once, for the same reason the nginx configuration
// this replaces declared them once at server level: an `add_header` inside an
// nginx location block discards the entire inherited set, which silently drops
// security headers exactly where they matter. Go has no such trap, but keeping
// one list keeps the property obvious.
var SecurityHeaders = map[string]string{
	"X-Content-Type-Options": "nosniff",
	"X-Frame-Options":        "SAMEORIGIN",
	"Referrer-Policy":        "same-origin",
	"Permissions-Policy":     "geolocation=(), microphone=(), camera=()",
}

// Handler serves the site.
func (s *Site) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, value := range SecurityHeaders {
			w.Header().Set(name, value)
		}

		// Resolve first, then check the method: 405 means "this resource
		// exists but not with that verb", so a POST to a path that is not
		// here at all must still be 404.
		f, ok := s.lookup(r.URL.Path)
		if !ok {
			s.serveNotFound(w, r)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serve(w, r, f, cacheControl(r.URL.Path), http.StatusOK)
	})
}

func (s *Site) serveNotFound(w http.ResponseWriter, r *http.Request) {
	if s.notFound == nil {
		w.Header().Set("Cache-Control", "no-cache")
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	s.serve(w, r, s.notFound, "no-cache", http.StatusNotFound)
}

func (s *Site) serve(w http.ResponseWriter, r *http.Request, f *file, cache string, status int) {
	header := w.Header()
	header.Set("Content-Type", f.contentType)
	header.Set("Cache-Control", cache)
	header.Set("ETag", f.etag)
	header.Set("Vary", "Accept-Encoding")

	// A conditional request is answered before anything is read or written,
	// which is the whole point of the content-hash ETag.
	if status == http.StatusOK && matchesETag(r.Header.Get("If-None-Match"), f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if f.gzipped != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		header.Set("Content-Encoding", "gzip")
		// http.ServeContent would negotiate Range against the compressed
		// bytes, which is not what a Range header means. Serve it whole.
		header.Set("Content-Length", itoa(len(f.gzipped)))
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(f.gzipped)
		}
		return
	}

	handle, err := os.Open(f.abs)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer handle.Close()

	if status != http.StatusOK {
		header.Set("Content-Length", itoa64(f.size))
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = copyTo(w, handle)
		}
		return
	}
	// ServeContent handles Range and If-Modified-Since for the plain case.
	http.ServeContent(w, r, f.abs, f.modTime, handle)
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		token := strings.TrimSpace(part)
		if name, rest, found := strings.Cut(token, ";"); found {
			// An explicit q=0 is a refusal, not an acceptance.
			if strings.Contains(strings.ReplaceAll(rest, " ", ""), "q=0") &&
				!strings.Contains(strings.ReplaceAll(rest, " ", ""), "q=0.") {
				continue
			}
			token = strings.TrimSpace(name)
		}
		if token == "gzip" || token == "*" {
			return true
		}
	}
	return false
}

// matchesETag implements the If-None-Match comparison: a list of tags, or "*".
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

func gzipBytes(content []byte) ([]byte, bool) {
	var buf bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, false
	}
	if _, err := writer.Write(content); err != nil {
		return nil, false
	}
	if err := writer.Close(); err != nil {
		return nil, false
	}
	// Storing a "compressed" copy that is no smaller only wastes memory.
	if buf.Len() >= len(content) {
		return nil, false
	}
	return buf.Bytes(), true
}
