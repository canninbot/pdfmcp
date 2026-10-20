package search

import (
	"context"
	"slices"
	"strings"
	"testing"

	indexdb "github.com/canninbot/pdfmcp/internal/index"
	"github.com/canninbot/pdfmcp/internal/testutil"
)

func TestRankedLiteralRegexScopesAndPagination(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := indexdb.Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	engine := New(database)
	tests := []struct {
		name    string
		options Options
		want    int
		typeOf  string
	}{
		{name: "ranked", options: Options{Query: `"TEAD inhibition"`, Mode: "ranked", Scope: "text"}, want: 2, typeOf: "text"},
		{name: "literal", options: Options{Query: "tead inhibition", Mode: "literal", Scope: "text"}, want: 2, typeOf: "text"},
		{name: "regex", options: Options{Query: `TEAD\s+inhibition`, Mode: "regex", Scope: "text"}, want: 2, typeOf: "text"},
		{name: "figure", options: Options{Query: "pathway", Mode: "literal", Scope: "figures"}, want: 2, typeOf: "figure"},
		{name: "table", options: Options{Query: "inhibitor", Mode: "literal", Scope: "tables"}, want: 2, typeOf: "table"},
		{name: "regex prefilter", options: Options{Query: `\bpathway\b`, Mode: "regex", Scope: "figures"}, want: 2, typeOf: "figure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := engine.Search(ctx, test.options)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Results) != test.want {
				t.Fatalf("got %d results: %+v", len(response.Results), response.Results)
			}
			if response.Results[0].ContentType != test.typeOf {
				t.Fatalf("unexpected type: %+v", response.Results[0])
			}
		})
	}
	page, err := engine.Search(ctx, Options{
		Query: "TEAD", Mode: "literal", Scope: "text", Limit: 1, Offset: 1,
	})
	if err != nil || len(page.Results) != 1 || page.Results[0].PMID != "23456789" {
		t.Fatalf("pagination: %+v, %v", page, err)
	}
}

func TestOutputSpecificLimits(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		limit   int
		wantErr string
	}{
		{name: "maximum PMIDs", output: "pmids", limit: 5_000},
		{name: "too many PMIDs", output: "pmids", limit: 5_001, wantErr: "5000"},
		{name: "maximum snippets", output: "snippets", limit: 200},
		{name: "too many snippets", output: "snippets", limit: 201, wantErr: "200"},
		{name: "maximum full", output: "full", limit: 200},
		{name: "too many full", output: "full", limit: 201, wantErr: "200"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := Options{Query: "PARP1", Mode: "literal", Scope: "text", Output: test.output, Limit: test.limit}
			err := normalizeOptions(&options)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("got error %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestOutputLevelsAndExactSemantics(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := indexdb.Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	engine := New(database)
	tests := []struct {
		name          string
		query         string
		caseSensitive bool
		want          []string
	}{
		{name: "substring", query: "sin3", want: []string{"12345678"}},
		{name: "full identifier", query: "SIN3A", want: []string{"12345678"}},
		{name: "clinical trial", query: "NCT04350463", want: []string{"12345678"}},
		{name: "punctuation", query: "IL-6", want: []string{"12345678"}},
		{name: "unicode", query: "β-catenin", want: []string{"12345678"}},
		{name: "short", query: "p5", want: []string{"12345678"}},
		{name: "case sensitive miss", query: "sin3", caseSensitive: true, want: []string{}},
		{name: "no match", query: "definitely-absent", want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, searchErr := engine.Search(ctx, Options{
				Query: test.query, Mode: "literal", Scope: "all", Output: "pmids",
				CaseSensitive: test.caseSensitive, Limit: 200,
			})
			if searchErr != nil {
				t.Fatal(searchErr)
			}
			if !slices.Equal(response.PMIDs, test.want) {
				t.Fatalf("got %v, want %v", response.PMIDs, test.want)
			}
			if len(response.Results) != 0 {
				t.Fatalf("PMID output constructed full results: %+v", response.Results)
			}
		})
	}

	snippets, err := engine.Search(ctx, Options{
		Query: "sin3", Mode: "literal", Scope: "all", Output: "snippets", Limit: 200,
	})
	if err != nil || len(snippets.Results) != 1 {
		t.Fatalf("snippets: %+v, %v", snippets, err)
	}
	if snippets.Results[0].Title != "" || snippets.Results[0].Snippet == "" || snippets.Results[0].Page == nil {
		t.Fatalf("unexpected snippet result: %+v", snippets.Results[0])
	}

	page, err := engine.Search(ctx, Options{
		Query: "TEAD", Mode: "literal", Scope: "all", Output: "pmids", Limit: 1, Offset: 1,
	})
	if err != nil || !slices.Equal(page.PMIDs, []string{"23456789"}) {
		t.Fatalf("PMID pagination: %+v, %v", page, err)
	}

	foldedRegex, err := engine.Search(ctx, Options{
		Query: "sin3", Mode: "regex", Scope: "all", Output: "pmids", Limit: 200,
	})
	if err != nil || !slices.Equal(foldedRegex.PMIDs, []string{"12345678", "23456789"}) {
		t.Fatalf("Unicode regex folding: %+v, %v", foldedRegex, err)
	}
}
