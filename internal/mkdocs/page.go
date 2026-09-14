// Package mkdocs reads a MkDocs project: its configuration, its navigation and
// the Markdown sources behind it.
//
// Nothing here renders HTML. An agent wants the source Markdown - that is what
// the author wrote, what it would have to edit, and what survives a theme
// change.
package mkdocs

import (
	"bufio"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Section is one heading and the prose beneath it, down to the next heading of
// the same or higher level.
//
// Sections, not pages, are the unit this server indexes and returns. A
// specification page here runs to thousands of lines; handing all of it back
// for a three-word question spends the agent's context on text it did not ask
// for.
type Section struct {
	// Level is the heading depth, 1 for `#`. The synthetic lead section that
	// holds text before the first heading has level 0.
	Level int
	// Title is the heading text with Markdown inline markup left intact.
	Title string
	// Anchor is the fragment MkDocs generates for this heading.
	Anchor string
	// Trail is the chain of enclosing headings, outermost first, excluding
	// this one.
	Trail []string
	// Body is the Markdown under the heading, heading line excluded.
	Body string
	// Line is the 1-based line number of the heading in the source file.
	Line int
}

// Breadcrumb renders the heading trail plus this section's own title.
func (s Section) Breadcrumb() string {
	if len(s.Trail) == 0 {
		return s.Title
	}
	if s.Title == "" {
		return strings.Join(s.Trail, " > ")
	}
	return strings.Join(append(append([]string{}, s.Trail...), s.Title), " > ")
}

var (
	atxHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	fenceMarker  = regexp.MustCompile("^\\s{0,3}(```+|~~~+)")
	nonSlugRunes = regexp.MustCompile(`[^\w\s-]`)
	slugSpaces   = regexp.MustCompile(`[\s-]+`)
)

// StripFrontMatter removes a leading YAML front-matter block and reports how
// many lines it consumed, so heading line numbers stay true to the file.
func StripFrontMatter(text string) (string, int) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return text, 0
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimRight(lines[i], "\r")
		if trimmed == "---" || trimmed == "..." {
			return strings.Join(lines[i+1:], "\n"), i + 1
		}
	}
	return text, 0
}

// Slugify reproduces the anchor Python-Markdown's toc extension generates,
// which is what MkDocs links and what a reader's URL fragment will say.
//
// The algorithm is: NFKD-normalise, drop everything outside ASCII, remove
// characters that are neither word characters, whitespace nor hyphens,
// lowercase, then collapse runs of whitespace and hyphens into one hyphen.
func Slugify(heading string) string {
	value := norm.NFKD.String(stripInlineMarkup(heading))
	var ascii strings.Builder
	for _, r := range value {
		if r < unicode.MaxASCII {
			ascii.WriteRune(r)
		}
	}
	value = nonSlugRunes.ReplaceAllString(ascii.String(), "")
	value = strings.ToLower(strings.TrimSpace(value))
	return slugSpaces.ReplaceAllString(value, "-")
}

// stripInlineMarkup removes the Markdown decorations that do not appear in the
// rendered heading text, so `## The `cf` client` anchors as `the-cf-client`.
var (
	inlineCode = regexp.MustCompile("`([^`]*)`")
	inlineLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	inlineStar = regexp.MustCompile(`\*{1,3}`)
)

func stripInlineMarkup(s string) string {
	s = inlineCode.ReplaceAllString(s, "$1")
	s = inlineLink.ReplaceAllString(s, "$1")
	s = inlineStar.ReplaceAllString(s, "")
	return stripEmphasisUnderscores(s)
}

// stripEmphasisUnderscores removes underscore runs used as emphasis but keeps
// the ones inside an identifier.
//
// Markdown does not treat an intra-word underscore as emphasis, so a heading
// named IMAGE_VERSION anchors as image_version. Removing it unconditionally
// would produce imageversion and every link to that heading would break.
func stripEmphasisUnderscores(s string) string {
	runes := []rune(s)
	var out []rune
	for i := 0; i < len(runes); {
		if runes[i] != '_' {
			out = append(out, runes[i])
			i++
			continue
		}
		run := i
		for run < len(runes) && runes[run] == '_' {
			run++
		}
		wordBefore := i > 0 && isWordRune(runes[i-1])
		wordAfter := run < len(runes) && isWordRune(runes[run])
		if wordBefore && wordAfter {
			out = append(out, runes[i:run]...)
		}
		i = run
	}
	return string(out)
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// Split cuts a Markdown document into sections.
//
// Fenced code blocks are tracked, because a `# comment` on the first column of
// a shell example is not a heading and indexing it as one would scatter the
// navigation with nonsense.
//
// Anchors are deduplicated the way Python-Markdown does it, by appending _1,
// _2 and so on, so two identically named headings keep distinct links.
func Split(source string) []Section {
	body, offset := StripFrontMatter(source)

	type crumb struct {
		level int
		title string
	}

	var (
		sections []Section
		current  = Section{Level: 0, Line: offset + 1}
		builder  strings.Builder
		stack    []crumb
		seen     = map[string]int{}
		fence    string
		lineNo   = offset
	)

	flush := func() {
		current.Body = strings.Trim(builder.String(), "\n")
		if current.Title != "" || strings.TrimSpace(current.Body) != "" {
			sections = append(sections, current)
		}
		builder.Reset()
	}

	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lineNo++

		if marker := fenceMarker.FindStringSubmatch(line); marker != nil {
			switch {
			case fence == "":
				fence = marker[1][:1]
			case strings.HasPrefix(marker[1], fence):
				fence = ""
			}
			builder.WriteString(line)
			builder.WriteString("\n")
			continue
		}
		if fence != "" {
			builder.WriteString(line)
			builder.WriteString("\n")
			continue
		}

		match := atxHeading.FindStringSubmatch(line)
		if match == nil {
			builder.WriteString(line)
			builder.WriteString("\n")
			continue
		}

		flush()

		level := len(match[1])
		title := match[2]

		// Pop every heading at this level or deeper: they are siblings or
		// children, not ancestors. What remains is the trail.
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			stack = stack[:len(stack)-1]
		}
		trail := make([]string, len(stack))
		for i, c := range stack {
			trail[i] = c.title
		}

		anchor := Slugify(title)
		if n, clash := seen[anchor]; clash {
			seen[anchor] = n + 1
			anchor = anchor + "_" + itoa(n)
		} else {
			seen[anchor] = 1
		}

		current = Section{
			Level:  level,
			Title:  title,
			Anchor: anchor,
			Trail:  trail,
			Line:   lineNo,
		}
		stack = append(stack, crumb{level: level, title: title})
	}
	flush()
	return sections
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
