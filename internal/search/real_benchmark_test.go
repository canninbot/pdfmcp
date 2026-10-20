package search

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/canninbot/pdfmcp/internal/config"
	indexdb "github.com/canninbot/pdfmcp/internal/index"
)

// benchmarkRealSearch measures an already-built real corpus index. Set
// PDFMCP_BENCH_ROOT to opt in; ordinary test runs remain self-contained.
func benchmarkRealSearch(b *testing.B, options Options) {
	b.Helper()
	root := os.Getenv("PDFMCP_BENCH_ROOT")
	if root == "" {
		b.Skip("set PDFMCP_BENCH_ROOT to benchmark a real corpus")
	}
	cfg, err := config.New(root, "")
	if err != nil {
		b.Fatal(err)
	}
	database, err := indexdb.Open(context.Background(), cfg, true)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	engine := New(database)
	if _, err := engine.Search(context.Background(), options); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		response, searchErr := engine.Search(context.Background(), options)
		if searchErr != nil {
			b.Fatal(searchErr)
		}
		if len(response.Results) == 0 && len(response.PMIDs) == 0 {
			b.Fatal("benchmark query unexpectedly returned no results")
		}
	}
}

func BenchmarkRealLiteralFull(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "literal", Scope: "all", Limit: 200,
	})
}

func BenchmarkRealLiteralPMIDsText(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "literal", Scope: "text", Output: "pmids", Limit: 200,
	})
}

func BenchmarkRealLiteralPMIDsAll(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "literal", Scope: "all", Output: "pmids", Limit: 200,
	})
}

func BenchmarkRealLiteralSnippets(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "literal", Scope: "all", Output: "snippets", Limit: 200,
	})
}

func BenchmarkRealLiteralSnippetsText(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "literal", Scope: "text", Output: "snippets", Limit: 200,
	})
}

func BenchmarkRealRegexFull(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "regex", Scope: "all", Limit: 200,
	})
}

func BenchmarkRealRegexPMIDsAll(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "regex", Scope: "all", Output: "pmids", Limit: 200,
	})
}

func BenchmarkRealRankedFull(b *testing.B) {
	benchmarkRealSearch(b, Options{
		Query: "sin3", Mode: "ranked", Scope: "all", Limit: 200,
	})
}

func BenchmarkRealLiteralFullJSON(b *testing.B) {
	root := os.Getenv("PDFMCP_BENCH_ROOT")
	if root == "" {
		b.Skip("set PDFMCP_BENCH_ROOT to benchmark a real corpus")
	}
	cfg, err := config.New(root, "")
	if err != nil {
		b.Fatal(err)
	}
	database, err := indexdb.Open(context.Background(), cfg, true)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	engine := New(database)
	options := Options{Query: "sin3", Mode: "literal", Scope: "all", Limit: 200}
	if _, err := engine.Search(context.Background(), options); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		response, searchErr := engine.Search(context.Background(), options)
		if searchErr != nil {
			b.Fatal(searchErr)
		}
		encoded, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			b.Fatal(marshalErr)
		}
		b.SetBytes(int64(len(encoded)))
	}
}

func BenchmarkRealLiteralFullMarshalOnly(b *testing.B) {
	root := os.Getenv("PDFMCP_BENCH_ROOT")
	if root == "" {
		b.Skip("set PDFMCP_BENCH_ROOT to benchmark a real corpus")
	}
	cfg, err := config.New(root, "")
	if err != nil {
		b.Fatal(err)
	}
	database, err := indexdb.Open(context.Background(), cfg, true)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	response, err := New(database).Search(context.Background(), Options{
		Query: "sin3", Mode: "literal", Scope: "all", Limit: 200,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		encoded, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			b.Fatal(marshalErr)
		}
		b.SetBytes(int64(len(encoded)))
	}
}
