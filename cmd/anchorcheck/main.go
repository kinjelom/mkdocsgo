// Command anchorcheck compares the anchors this server computes against the
// anchors MkDocs actually rendered into a built site.
//
// The anchors are reimplemented here rather than taken from MkDocs, so they
// can drift if Python-Markdown changes how it slugifies a heading. Run this
// against a freshly built site to find out before an agent follows a dead
// link:
//
//	mkdocs build
//	anchorcheck -project . -site site
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kinjelom/mkdocsgo/internal/mkdocs"
)

// The quotes are optional because a minifying MkDocs plugin strips them.
var headingID = regexp.MustCompile(`<h[1-6][^>]*\sid=["']?([^"'\s>]+)`)

func htmlPath(siteDir, page string) string {
	rel := strings.TrimSuffix(page, ".md")
	switch {
	case rel == "index":
		return filepath.Join(siteDir, "index.html")
	case strings.HasSuffix(rel, "/index"):
		return filepath.Join(siteDir, filepath.FromSlash(rel)+".html")
	default:
		return filepath.Join(siteDir, filepath.FromSlash(rel), "index.html")
	}
}

func main() {
	project := flag.String("project", ".", "path to the MkDocs project")
	site := flag.String("site", "site", "path to the built site")
	flag.Parse()

	loaded, err := mkdocs.Load(*project)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	var pages, compared, mismatched, skipped int
	for _, page := range loaded.Pages {
		data, err := os.ReadFile(htmlPath(*site, page.Path))
		if err != nil {
			skipped++
			continue
		}
		pages++

		rendered := map[string]bool{}
		for _, match := range headingID.FindAllStringSubmatch(string(data), -1) {
			rendered[match[1]] = true
		}
		for _, section := range page.Sections {
			if section.Anchor == "" {
				continue
			}
			compared++
			if !rendered[section.Anchor] {
				mismatched++
				fmt.Printf("MISMATCH %s -> %q (heading %q)\n", page.Path, section.Anchor, section.Title)
			}
		}
	}

	fmt.Printf("%d pages, %d anchors compared, %d mismatched", pages, compared, mismatched)
	if skipped > 0 {
		fmt.Printf(", %d pages skipped (no built HTML)", skipped)
	}
	fmt.Println()
	if mismatched > 0 {
		os.Exit(1)
	}
}
