package search

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	indexdb "github.com/canninbot/pdfmcp/internal/index"
	"github.com/canninbot/pdfmcp/internal/testutil"
)

func TestFilesystemSearchWithoutIndex(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	engine, err := NewFilesystem(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	response, err := engine.Search(ctx, Options{
		Query: "tead inhibition", Mode: "literal", Scope: "text", Output: "pmids",
	})
	if err != nil || !slices.Equal(response.PMIDs, []string{"12345678", "23456789"}) {
		t.Fatalf("indexless literal search: %+v, %v", response, err)
	}
	response, err = engine.Search(ctx, Options{
		Query: `TEAD\s+inhibition`, Mode: "regex", Scope: "text", Limit: 1, Offset: 1,
	})
	if err != nil || len(response.Results) != 1 || response.Results[0].PMID != "23456789" {
		t.Fatalf("indexless regex pagination: %+v, %v", response, err)
	}
	for _, options := range []Options{
		{Query: "TEAD", Mode: "ranked", Scope: "text"},
		{Query: "TEAD", Mode: "literal", Scope: "all"},
		{Query: "TEAD", Mode: "literal", Scope: "metadata"},
	} {
		if _, err := engine.Search(ctx, options); err == nil {
			t.Fatalf("unsupported indexless options succeeded: %+v", options)
		}
	}
	if _, err := os.Stat(cfg.Database); !os.IsNotExist(err) {
		t.Fatalf("indexless search created a database: %v", err)
	}
}

func TestMarkdownReaderMatchesLongAndFinalLines(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		options Options
		regex   *regexp.Regexp
		matcher *lowerMatcher
	}{
		{
			name:    "literal beyond buffer",
			data:    append(bytes.Repeat([]byte{'x'}, 70*1024), []byte("TaRgEt\n")...),
			options: Options{Query: "target", Mode: "literal"},
			matcher: newLowerMatcher("target"),
		},
		{
			name:    "regex on final line",
			data:    []byte("first line\r\nfinal 123"),
			options: Options{Query: `final\s+[0-9]+`, Mode: "regex"},
			regex:   regexp.MustCompile(`(?i)final\s+[0-9]+`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := bufio.NewReaderSize(bytes.NewReader(test.data), 64*1024)
			matched, err := markdownReaderMatches(
				reader, test.options, test.regex, test.matcher, &bytes.Buffer{},
			)
			if err != nil || !matched {
				t.Fatalf("matched=%t, err=%v", matched, err)
			}
		})
	}
}

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

func TestOptionalMetadataSearchIsSeparate(t *testing.T) {
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
	annotations := filepath.Join(t.TempDir(), "annotations.csv")
	testutil.WriteAnnotationsCSV(t, annotations)
	if _, err := database.ImportAnnotations(ctx, annotations); err != nil {
		t.Fatal(err)
	}
	engine := New(database)
	tests := []struct {
		name    string
		options Options
		want    string
	}{
		{name: "ranked", options: Options{Query: `"Clinical Trial"`, Mode: "ranked", Scope: "metadata"}, want: "12345678"},
		{name: "literal", options: Options{Query: "Q3", Mode: "literal", Scope: "metadata"}, want: "23456789"},
		{name: "regex", options: Options{Query: `Randomized\s+Controlled`, Mode: "regex", Scope: "metadata"}, want: "12345678"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := engine.Search(ctx, test.options)
			if err != nil || len(response.Results) != 1 || response.Results[0].PMID != test.want ||
				response.Results[0].ContentType != "metadata" ||
				response.Results[0].ResourceURI != "paper://"+test.want+"/annotations" {
				t.Fatalf("unexpected metadata search: %+v, %v", response, err)
			}
		})
	}
	defaultSearch, err := engine.Search(ctx, Options{
		Query: "Clinical Trial", Mode: "literal", Scope: "all",
	})
	if err != nil || len(defaultSearch.Results) != 0 {
		t.Fatalf("scope=all included optional metadata: %+v, %v", defaultSearch, err)
	}
	pmids, err := engine.Search(ctx, Options{
		Query: "Synthetic Reviews", Mode: "literal", Scope: "metadata", Output: "pmids",
	})
	if err != nil || !slices.Equal(pmids.PMIDs, []string{"23456789"}) {
		t.Fatalf("metadata PMID output: %+v, %v", pmids, err)
	}
	warningFields := make(map[string]struct{}, len(pmids.CompletenessWarnings))
	for _, warning := range pmids.CompletenessWarnings {
		warningFields[warning.Field] = struct{}{}
	}
	if _, ok := warningFields["study_designs"]; !ok {
		t.Fatalf("metadata search omitted study_designs warning: %+v", pmids)
	}
	if _, ok := warningFields["pmcid"]; ok {
		t.Fatalf("metadata search warned for exempt pmcid: %+v", pmids)
	}
	filtered, err := engine.Search(ctx, Options{
		Scope: "metadata", Output: "pmids",
		MetadataFilters: []MetadataFilter{{Field: "cited_by_count", Operator: ">", Value: 100}},
	})
	if err != nil || !slices.Equal(filtered.PMIDs, []string{"12345678"}) {
		t.Fatalf("filter-only metadata output: %+v, %v", filtered, err)
	}
	combined, err := engine.Search(ctx, Options{
		Query: `"Clinical Trial"`, Mode: "ranked", Scope: "metadata",
		MetadataFilters: []MetadataFilter{{Field: "cited_by_count", Operator: ">", Value: 100}},
	})
	if err != nil || len(combined.Results) != 1 || combined.Results[0].PMID != "12345678" {
		t.Fatalf("combined metadata filter: %+v, %v", combined, err)
	}
}

func TestMetadataFilterParsingAndValidation(t *testing.T) {
	filter, err := ParseMetadataFilter("cited_by_count > 100")
	if err != nil || filter != (MetadataFilter{Field: "cited_by_count", Operator: ">", Value: 100}) {
		t.Fatalf("parsed filter: %+v, %v", filter, err)
	}
	for _, expression := range []string{"title=10", "cited_by_count~100", "cited_by_count>many"} {
		if _, err := ParseMetadataFilter(expression); err == nil {
			t.Errorf("ParseMetadataFilter(%q) succeeded", expression)
		}
	}
	options := Options{
		Query: "TEAD", Scope: "text",
		MetadataFilters: []MetadataFilter{{Field: "cited_by_count", Operator: ">", Value: 100}},
	}
	if err := normalizeOptions(&options); err == nil || !strings.Contains(err.Error(), "scope metadata") {
		t.Fatalf("metadata filter was accepted for text scope: %v", err)
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

func TestRankedPMIDOutput(t *testing.T) {
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
	firstPage, err := engine.Search(ctx, Options{
		Query: `"TEAD inhibition"`, Mode: "ranked", Scope: "text",
		Output: "pmids", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage.PMIDs) != 2 || firstPage.PMIDs[0] == firstPage.PMIDs[1] {
		t.Fatalf("unexpected ranked PMIDs: %+v", firstPage)
	}
	secondPage, err := engine.Search(ctx, Options{
		Query: `"TEAD inhibition"`, Mode: "ranked", Scope: "text",
		Output: "pmids", Limit: 1, Offset: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(secondPage.PMIDs, firstPage.PMIDs[1:]) {
		t.Fatalf("pagination mismatch: first=%+v second=%+v", firstPage, secondPage)
	}
	if len(secondPage.Results) != 0 {
		t.Fatalf("PMID output constructed full results: %+v", secondPage.Results)
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
