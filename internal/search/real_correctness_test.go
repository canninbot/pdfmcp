package search

import (
	"context"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/canninbot/pdfmcp/internal/config"
	indexdb "github.com/canninbot/pdfmcp/internal/index"
)

// TestRealCorpusExactPMIDs compares accelerated results with a single-pass,
// non-indexed scan. Set PDFMCP_TEST_ROOT to opt in on a real corpus.
func TestRealCorpusExactPMIDs(t *testing.T) {
	root := os.Getenv("PDFMCP_TEST_ROOT")
	if root == "" {
		t.Skip("set PDFMCP_TEST_ROOT to verify a real corpus")
	}
	cfg, err := config.New(root, "")
	if err != nil {
		t.Fatal(err)
	}
	database, err := indexdb.Open(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	tests := []struct {
		query string
		mode  string
		scope string
	}{
		{query: "sin3", mode: "literal", scope: "all"},
		{query: "SIN3A", mode: "literal", scope: "all"},
		{query: "NCT04350463", mode: "literal", scope: "all"},
		{query: "TEAD", mode: "literal", scope: "all"},
		{query: "KRAS", mode: "literal", scope: "all"},
		{query: "IL-6", mode: "literal", scope: "all"},
		{query: "p53", mode: "literal", scope: "all"},
		{query: "β-catenin", mode: "literal", scope: "all"},
		{query: "thrombocytopenia", mode: "literal", scope: "all"},
		{query: "pathway", mode: "literal", scope: "figures"},
		{query: "inhibitor", mode: "literal", scope: "tables"},
		{query: `TEAD\s+`, mode: "regex", scope: "all"},
		{query: `\bpathway\b`, mode: "regex", scope: "figures"},
	}
	expected := make([]map[string]struct{}, len(tests))
	regexes := make([]*regexp.Regexp, len(tests))
	for position, test := range tests {
		expected[position] = make(map[string]struct{})
		if test.mode == "regex" {
			regexes[position] = regexp.MustCompile("(?i)" + test.query)
		}
	}
	rows, err := database.SQL().QueryContext(context.Background(), `
		SELECT pmid, block_type, searchable_text FROM blocks ORDER BY pmid, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var pmid, blockType, searchable string
		if err := rows.Scan(&pmid, &blockType, &searchable); err != nil {
			t.Fatal(err)
		}
		lowered := strings.ToLower(searchable)
		for position, test := range tests {
			if !scopeMatches(test.scope, blockType) {
				continue
			}
			matched := false
			if test.mode == "literal" {
				matched = strings.Contains(lowered, strings.ToLower(test.query))
			} else {
				for _, line := range strings.Split(strings.ReplaceAll(searchable, "\r\n", "\n"), "\n") {
					if regexes[position].MatchString(line) {
						matched = true
						break
					}
				}
			}
			if matched {
				expected[position][pmid] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	engine := New(database)
	for position, test := range tests {
		t.Run(test.mode+"/"+test.scope+"/"+test.query, func(t *testing.T) {
			want := make([]string, 0, len(expected[position]))
			for pmid := range expected[position] {
				want = append(want, pmid)
			}
			sort.Strings(want)
			got := make([]string, 0, len(want))
			for offset := 0; ; offset += config.MaxPMIDQueryLimit {
				response, searchErr := engine.Search(context.Background(), Options{
					Query: test.query, Mode: test.mode, Scope: test.scope,
					Output: "pmids", Limit: config.MaxPMIDQueryLimit, Offset: offset,
				})
				if searchErr != nil {
					t.Fatal(searchErr)
				}
				got = append(got, response.PMIDs...)
				if len(response.PMIDs) < config.MaxPMIDQueryLimit {
					break
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got %d PMIDs, want %d", len(got), len(want))
			}
		})
	}
}

func scopeMatches(scope, blockType string) bool {
	switch scope {
	case "figures":
		return blockType == "figure"
	case "tables":
		return blockType == "table"
	default:
		return true
	}
}
