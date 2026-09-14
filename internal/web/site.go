// Package web serves a site built by `mkdocs build`.
//
// It replaces the nginx container that would otherwise sit in front of the
// same files, and reproduces the parts of that configuration that matter:
// security headers declared once, a per-path cache policy, gzip, directory
// URLs and the project's own 404 page.
//
// Two things here are better than the nginx setup it replaces. Compression
// happens once at startup rather than per request, and ETags are content
// hashes rather than nginx's inode-and-mtime pair - so two instances serving
// the same build return the same ETag, and a client that reaches a different
// instance revalidates instead of downloading again.
package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Only compress what compresses. Images, fonts and archives are already
// compact, and running them through gzip costs memory to save nothing.
var compressible = map[string]bool{
	".html": true, ".css": true, ".js": true, ".mjs": true,
	".json": true, ".map": true, ".svg": true, ".xml": true,
	".txt": true, ".md": true, ".webmanifest": true,
}

// A file smaller than this is not worth compressing: the gzip header and the
// Vary bookkeeping cost more than they save.
const minCompressSize = 256

type file struct {
	abs         string
	size        int64
	modTime     time.Time
	contentType string
	etag        string
	gzipped     []byte // nil when not stored compressed
}

// Site is an immutable view of a built site, safe for concurrent use.
type Site struct {
	root     string
	files    map[string]*file
	notFound *file
	rawBytes int64
	gzBytes  int64
}

// Load indexes every file under dir. The directory is read once; later edits
// are not picked up, which matches how the site is shipped - baked into an
// image next to this binary.
func Load(dir string) (*Site, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("site directory %q does not exist (run `mkdocs build` first)", dir)
	}

	site := &Site{root: root, files: map[string]*file{}}
	err = filepath.WalkDir(root, func(abs string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			return err
		}

		sum := sha256.Sum256(content)
		ext := strings.ToLower(filepath.Ext(abs))
		f := &file{
			abs:         abs,
			size:        stat.Size(),
			modTime:     stat.ModTime(),
			contentType: contentTypeFor(ext),
			etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
		}
		if compressible[ext] && stat.Size() >= minCompressSize {
			if gz, ok := gzipBytes(content); ok {
				f.gzipped = gz
				site.gzBytes += int64(len(gz))
			}
		}
		site.rawBytes += stat.Size()
		site.files["/"+filepath.ToSlash(rel)] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	site.notFound = site.files["/404.html"]
	return site, nil
}

// Len reports how many files are served.
func (s *Site) Len() int { return len(s.files) }

// Bytes reports the size on disk and the size held compressed in memory.
func (s *Site) Bytes() (raw, compressed int64) { return s.rawBytes, s.gzBytes }

// lookup resolves a request path the way MkDocs expects it to resolve:
// an exact file, then the directory's index.html.
func (s *Site) lookup(urlPath string) (*file, bool) {
	clean := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	if f, ok := s.files[clean]; ok {
		return f, true
	}
	// MkDocs publishes directory URLs: /page/ is /page/index.html.
	if f, ok := s.files[path.Join(clean, "index.html")]; ok {
		return f, true
	}
	return nil, false
}

func contentTypeFor(ext string) string {
	switch ext {
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".map":
		return "application/json; charset=utf-8"
	case ".webmanifest":
		return "application/manifest+json; charset=utf-8"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// cacheControl mirrors the policy the nginx configuration this replaces spelled
// out in a map, and for the same reasons.
func cacheControl(urlPath string) string {
	switch {
	// Material fingerprints these filenames, so a changed file has a changed
	// name and the old one can be cached forever.
	case strings.HasPrefix(urlPath, "/assets/stylesheets/"),
		strings.HasPrefix(urlPath, "/assets/javascripts/"):
		return "public, max-age=31536000, immutable"
	// Vendored console bundles sit at an unversioned path, so a version bump
	// must still reach browsers within a day.
	case strings.HasPrefix(urlPath, "/assets/vendor/"):
		return "public, max-age=86400"
	}
	switch strings.ToLower(path.Ext(urlPath)) {
	// Not fingerprinted; a week balances traffic against fixing a diagram.
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".ico",
		".woff", ".woff2", ".ttf", ".otf":
		return "public, max-age=604800"
	}
	// HTML, openapi.json and the project's own css/js revalidate every time.
	// They are cheap to revalidate because the ETag is a content hash.
	return "no-cache"
}
