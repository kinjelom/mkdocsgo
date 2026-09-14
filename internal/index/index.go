// Package index is a small BM25 full-text index over documentation sections.
//
// It is deliberately hand-rolled and dependency-free. A documentation set is
// thousands of sections, not millions; at that size an inverted map and BM25
// scoring fit in a few hundred lines, start instantly, need no index files on
// disk and no cgo - which keeps the server a single static binary.
package index

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// Terms in a heading describe the whole section, so they count for more
	// than the same term buried in a paragraph.
	titleWeight = 3
	// A section whose text contains the query verbatim is almost always what
	// was meant, especially for identifiers like InvoiceNumber.
	phraseBonus = 2.5
)

// Doc is one indexed unit: a section of a page.
type Doc struct {
	ID    int
	Ref   any    // caller's handle, returned untouched on a Hit
	Title string // heading plus its trail and the page title
	Body  string
}

// Hit is a scored match.
type Hit struct {
	Doc     *Doc
	Score   float64
	Snippet string
}

type posting struct {
	doc  int
	freq float64
}

// Index is an immutable inverted index. Build it once with Build, then query
// it concurrently.
type Index struct {
	docs      []*Doc
	postings  map[string][]posting
	docLen    []float64
	avgDocLen float64
}

// Build indexes the documents. The caller keeps ownership of the slice.
func Build(docs []*Doc) *Index {
	ix := &Index{
		docs:     docs,
		postings: make(map[string][]posting, len(docs)*16),
		docLen:   make([]float64, len(docs)),
	}
	var total float64
	for i, doc := range docs {
		doc.ID = i
		counts := map[string]float64{}
		for _, term := range Tokenize(doc.Body) {
			counts[term]++
		}
		for _, term := range Tokenize(doc.Title) {
			counts[term] += titleWeight
		}
		var length float64
		for term, freq := range counts {
			ix.postings[term] = append(ix.postings[term], posting{doc: i, freq: freq})
			length += freq
		}
		ix.docLen[i] = length
		total += length
	}
	if len(docs) > 0 {
		ix.avgDocLen = total / float64(len(docs))
	}
	if ix.avgDocLen == 0 {
		ix.avgDocLen = 1
	}
	return ix
}

// Len reports how many documents are indexed.
func (ix *Index) Len() int { return len(ix.docs) }

// Search returns the best matches for a query, most relevant first.
func (ix *Index) Search(query string, limit int) []Hit {
	terms := Tokenize(query)
	if len(terms) == 0 || limit <= 0 {
		return nil
	}

	scores := make(map[int]float64, 64)
	n := float64(len(ix.docs))
	for _, term := range dedupe(terms) {
		postings := ix.postings[term]
		if len(postings) == 0 {
			continue
		}
		// BM25 inverse document frequency, the +1 keeping it positive even for
		// a term that appears in every document.
		df := float64(len(postings))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range postings {
			tf := p.freq
			norm := tf + bm25K1*(1-bm25B+bm25B*ix.docLen[p.doc]/ix.avgDocLen)
			scores[p.doc] += idf * (tf * (bm25K1 + 1)) / norm
		}
	}
	if len(scores) == 0 {
		return nil
	}

	phrase := strings.ToLower(strings.TrimSpace(query))
	hits := make([]Hit, 0, len(scores))
	for docID, score := range scores {
		doc := ix.docs[docID]
		if len(terms) > 1 && phrase != "" {
			if strings.Contains(strings.ToLower(doc.Title), phrase) ||
				strings.Contains(strings.ToLower(doc.Body), phrase) {
				score += phraseBonus
			}
		}
		hits = append(hits, Hit{Doc: doc, Score: score})
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Doc.ID < hits[j].Doc.ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		hits[i].Snippet = Snippet(hits[i].Doc.Body, terms, 320)
	}
	return hits
}

// Tokenize lowercases and splits on anything that is not a letter or digit.
//
// Two deliberate exceptions:
//
// A dot between two digits is kept, so a version like 1.0.0 stays one token
// and can be searched for. Splitting it would produce three single digits,
// which the length rule below then discards entirely.
//
// Single characters are otherwise dropped: they carry almost no signal and
// they dominate the postings list. Underscores split, so a heading named
// IMAGE_VERSION is found by searching for "version".
func Tokenize(text string) []string {
	var (
		tokens  []string
		builder strings.Builder
		runes   = []rune(text)
	)
	flush := func() {
		if builder.Len() >= 2 {
			tokens = append(tokens, builder.String())
		}
		builder.Reset()
	}
	for i, r := range runes {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(unicode.ToLower(r))
		case r == '.' && i > 0 && i+1 < len(runes) &&
			unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]):
			builder.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return tokens
}

func dedupe(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	out := terms[:0:0]
	for _, t := range terms {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// Snippet returns a window of body around the first query term found, trimmed
// to word boundaries, with ellipses where text was cut.
//
// Returning the window rather than the section keeps a result list readable:
// the agent sees why each hit matched and fetches the full section only for
// the one it wants.
func Snippet(body string, terms []string, width int) string {
	flat := strings.Join(strings.Fields(body), " ")
	if flat == "" {
		return ""
	}
	if len(flat) <= width {
		return flat
	}

	lower := strings.ToLower(flat)
	best := -1
	for _, term := range terms {
		if at := strings.Index(lower, term); at >= 0 && (best < 0 || at < best) {
			best = at
		}
	}
	if best < 0 {
		return trimToWords(flat[:width]) + " ..."
	}

	start := best - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(flat) {
		end = len(flat)
		if start = end - width; start < 0 {
			start = 0
		}
	}

	window := flat[start:end]
	if start > 0 {
		if cut := strings.IndexByte(window, ' '); cut >= 0 {
			window = window[cut+1:]
		}
		window = "... " + window
	}
	if end < len(flat) {
		window = trimToWords(window) + " ..."
	}
	return window
}

func trimToWords(s string) string {
	if cut := strings.LastIndexByte(s, ' '); cut > 0 {
		return s[:cut]
	}
	return s
}
