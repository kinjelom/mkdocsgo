package mkdocs

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Page is one Markdown source file and everything derived from it.
type Page struct {
	// Path is the file's location relative to docs_dir, slash-separated,
	// extension included: "guides/intro.md".
	Path string
	// Title is the navigation title when the file appears in nav:, otherwise
	// the first level-1 heading, otherwise the filename.
	Title string
	// NavTrail is the chain of navigation sections above this page.
	NavTrail []string
	// Source is the raw Markdown, front matter included.
	Source string
	// Sections is Source split by heading.
	Sections []Section
	// InNav reports whether the page is reachable from nav:.
	InNav bool
}

// URI is the resource identifier this page is served under.
func (p Page) URI() string { return "docs://" + p.Path }

// Breadcrumb renders the navigation path to the page.
func (p Page) Breadcrumb() string {
	if len(p.NavTrail) == 0 {
		return p.Title
	}
	return strings.Join(append(append([]string{}, p.NavTrail...), p.Title), " > ")
}

// Project is a loaded MkDocs project.
type Project struct {
	Root     string
	ConfigAt string
	SiteName string
	DocsDir  string
	Pages    []Page

	byPath map[string]*Page
}

// Page returns the page at a docs-relative path, tolerating a leading slash
// and a missing .md extension, both of which agents produce routinely.
func (p *Project) Page(relPath string) (*Page, bool) {
	clean := strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(relPath)), "/")
	if page, ok := p.byPath[clean]; ok {
		return page, true
	}
	if !strings.HasSuffix(clean, ".md") {
		if page, ok := p.byPath[clean+".md"]; ok {
			return page, true
		}
		if page, ok := p.byPath[path.Join(clean, "index.md")]; ok {
			return page, true
		}
	}
	return nil, false
}

// Load reads the MkDocs project rooted at dir.
func Load(dir string) (*Project, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	configAt := ""
	for _, name := range []string{"mkdocs.yml", "mkdocs.yaml"} {
		candidate := filepath.Join(root, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			configAt = candidate
			break
		}
	}
	if configAt == "" {
		return nil, fmt.Errorf("no mkdocs.yml or mkdocs.yaml in %s", root)
	}

	raw, err := os.ReadFile(configAt)
	if err != nil {
		return nil, err
	}

	// Decoding into a yaml.Node rather than a map is deliberate: mkdocs.yml
	// routinely carries Python-specific tags such as
	// !!python/name:material.extensions.emoji.twemoji, and it preserves the
	// order of nav:, which is the navigation.
	var file yaml.Node
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", configAt, err)
	}
	if len(file.Content) == 0 {
		return nil, fmt.Errorf("%s: empty configuration", configAt)
	}
	doc := file.Content[0]

	project := &Project{
		Root:     root,
		ConfigAt: configAt,
		SiteName: scalarField(doc, "site_name"),
		DocsDir:  scalarField(doc, "docs_dir"),
		byPath:   map[string]*Page{},
	}
	if project.SiteName == "" {
		project.SiteName = filepath.Base(root)
	}
	if project.DocsDir == "" {
		project.DocsDir = "docs"
	}
	docsDir := filepath.Join(root, filepath.FromSlash(project.DocsDir))
	if info, err := os.Stat(docsDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("docs_dir %q does not exist under %s", project.DocsDir, root)
	}

	excludes := parseExcludes(scalarField(doc, "exclude_docs"))
	navTitles, navTrails := walkNav(mappingValue(doc, "nav"), nil)

	err = filepath.WalkDir(docsDir, func(abs string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		rel, err := filepath.Rel(docsDir, abs)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if excluded(relSlash, excludes) {
			return nil
		}
		source, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		text := string(source)
		page := Page{
			Path:     relSlash,
			Source:   text,
			Sections: Split(text),
			Title:    navTitles[relSlash],
			NavTrail: navTrails[relSlash],
		}
		_, page.InNav = navTitles[relSlash]
		if page.Title == "" {
			page.Title = fallbackTitle(page)
		}
		project.Pages = append(project.Pages, page)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(project.Pages, func(i, j int) bool {
		return project.Pages[i].Path < project.Pages[j].Path
	})
	for i := range project.Pages {
		project.byPath[project.Pages[i].Path] = &project.Pages[i]
	}
	return project, nil
}

// fallbackTitle is used for a page the navigation does not name: its first
// level-1 heading, or failing that the file name.
func fallbackTitle(p Page) string {
	for _, s := range p.Sections {
		if s.Level == 1 {
			return stripInlineMarkup(s.Title)
		}
	}
	base := path.Base(p.Path)
	return strings.TrimSuffix(base, path.Ext(base))
}

func scalarField(mapping *yaml.Node, key string) string {
	node := mappingValue(mapping, key)
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// walkNav flattens nav: into a title and a trail for every page it mentions.
//
// MkDocs allows three shapes in a nav list: a bare path, a "Title: path" pair,
// and a "Title:" holding a nested list. All three appear in real projects.
func walkNav(node *yaml.Node, trail []string) (map[string]string, map[string][]string) {
	titles := map[string]string{}
	trails := map[string][]string{}
	if node == nil || node.Kind != yaml.SequenceNode {
		return titles, trails
	}
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			target := path.Clean(item.Value)
			titles[target] = strings.TrimSuffix(path.Base(target), path.Ext(target))
			trails[target] = append([]string{}, trail...)
		case yaml.MappingNode:
			for i := 0; i+1 < len(item.Content); i += 2 {
				label := item.Content[i].Value
				value := item.Content[i+1]
				switch value.Kind {
				case yaml.ScalarNode:
					target := path.Clean(value.Value)
					titles[target] = label
					trails[target] = append([]string{}, trail...)
				case yaml.SequenceNode:
					childTitles, childTrails := walkNav(value, append(append([]string{}, trail...), label))
					for k, v := range childTitles {
						titles[k] = v
					}
					for k, v := range childTrails {
						trails[k] = v
					}
				}
			}
		}
	}
	return titles, trails
}

// parseExcludes reads the exclude_docs block.
//
// MkDocs accepts gitignore syntax there. This understands the part projects
// actually use - one pattern per line, directory prefixes and shell globs -
// and ignores negation, which would silently include more than asked.
func parseExcludes(block string) []string {
	var patterns []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		patterns = append(patterns, strings.TrimPrefix(line, "/"))
	}
	return patterns
}

func excluded(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "/") {
			if strings.HasPrefix(relPath, pattern) {
				return true
			}
			continue
		}
		if relPath == pattern {
			return true
		}
		if ok, _ := path.Match(pattern, relPath); ok {
			return true
		}
		if ok, _ := path.Match(pattern, path.Base(relPath)); ok {
			return true
		}
	}
	return false
}
