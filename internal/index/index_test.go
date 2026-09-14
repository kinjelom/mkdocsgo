package index

import (
	"strings"
	"testing"
)

func build() *Index {
	return Build([]*Doc{
		{Ref: "deploy", Title: "Deployment Cloud Foundry rolling", Body: "A rolling deployment keeps the old instances serving until the new ones report healthy."},
		{Ref: "memory", Title: "Container size memory", Body: "The memory quota is 64M because nginx serves static files with one worker."},
		{Ref: "invoice", Title: "Invoice JSON InvoiceNumber", Body: "The InvoiceNumber field is mandatory in the header section of every invoice."},
		{Ref: "noise", Title: "Unrelated", Body: "Nothing here mentions the other topics at all, it is filler text about weather."},
	})
}

func TestSearchRanksTheRightSectionFirst(t *testing.T) {
	ix := build()
	for query, want := range map[string]string{
		"rolling deployment": "deploy",
		"memory quota":       "memory",
		"InvoiceNumber":      "invoice",
	} {
		hits := ix.Search(query, 3)
		if len(hits) == 0 {
			t.Fatalf("%q returned nothing", query)
		}
		if got := hits[0].Doc.Ref.(string); got != want {
			t.Errorf("%q ranked %q first, want %q", query, got, want)
		}
	}
}

func TestSearchPrefersHeadingMatches(t *testing.T) {
	ix := Build([]*Doc{
		{Ref: "heading", Title: "Rollback", Body: "Unrelated prose."},
		{Ref: "body", Title: "Something else", Body: "rollback appears only here in the body text."},
	})
	hits := ix.Search("rollback", 2)
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].Doc.Ref.(string) != "heading" {
		t.Error("a heading match should outrank a body match")
	}
}

func TestSearchRespectsLimitAndEmptyQuery(t *testing.T) {
	ix := build()
	if hits := ix.Search("the", 2); len(hits) > 2 {
		t.Errorf("limit ignored: got %d hits", len(hits))
	}
	if hits := ix.Search("   ", 5); hits != nil {
		t.Errorf("blank query returned %d hits, want none", len(hits))
	}
	if hits := ix.Search("zzzznotpresent", 5); hits != nil {
		t.Errorf("unmatched query returned %d hits, want none", len(hits))
	}
}

func TestTokenizeKeepsVersionNumbersWhole(t *testing.T) {
	for _, in := range []string{"1.0.0", "version 1.0.0 here"} {
		if !contains(Tokenize(in), "1.0.0") {
			t.Errorf("Tokenize(%q) lost the version: %v", in, Tokenize(in))
		}
	}
	// A sentence-ending dot must still split.
	if got := Tokenize("done. next"); len(got) != 2 {
		t.Errorf("Tokenize(\"done. next\") = %v, want two tokens", got)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestTokenizeDropsSingleCharactersAndSplitsIdentifiers(t *testing.T) {
	got := strings.Join(Tokenize("a IMAGE_VERSION=1.0.0, ok?"), " ")
	want := "image version 1.0.0 ok"
	if got != want {
		t.Errorf("Tokenize = %q, want %q", got, want)
	}
}

func TestSnippetCentresOnTheMatch(t *testing.T) {
	body := strings.Repeat("filler words here. ", 40) + "THE NEEDLE IS HERE. " + strings.Repeat("more filler. ", 40)
	got := Snippet(body, []string{"needle"}, 120)
	if !strings.Contains(strings.ToLower(got), "needle") {
		t.Fatalf("snippet does not contain the match: %q", got)
	}
	if len(got) > 160 {
		t.Errorf("snippet is %d chars, want roughly 120", len(got))
	}
	if !strings.HasPrefix(got, "... ") {
		t.Errorf("a snippet cut at the front should say so: %q", got)
	}
}

func TestSnippetShortBodyReturnedWhole(t *testing.T) {
	if got := Snippet("Short body.", []string{"short"}, 320); got != "Short body." {
		t.Errorf("Snippet = %q, want the whole body", got)
	}
}
