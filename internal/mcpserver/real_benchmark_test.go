package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/canninbot/pdfmcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func benchmarkRealMCP(b *testing.B, arguments map[string]any) {
	b.Helper()
	root := os.Getenv("PDFMCP_BENCH_ROOT")
	if root == "" {
		b.Skip("set PDFMCP_BENCH_ROOT to benchmark a real corpus")
	}
	cfg, err := config.New(root, "")
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	service, err := New(ctx, cfg, Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = service.Close() })
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := service.Server().Connect(ctx, serverTransport, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "pdfmcp-benchmark", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = clientSession.Close() })
	params := &mcp.CallToolParams{Name: "search", Arguments: arguments}
	if _, err := clientSession.CallTool(ctx, params); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, callErr := clientSession.CallTool(ctx, params)
		if callErr != nil {
			b.Fatal(callErr)
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			b.Fatal(marshalErr)
		}
		b.SetBytes(int64(len(encoded)))
	}
}

func BenchmarkRealMCPLiteralPMIDsText(b *testing.B) {
	benchmarkRealMCP(b, map[string]any{
		"query": "sin3", "mode": "literal", "scope": "text",
		"output": "pmids", "limit": 200,
	})
}

func BenchmarkRealMCPLiteralFull(b *testing.B) {
	benchmarkRealMCP(b, map[string]any{
		"query": "sin3", "mode": "literal", "scope": "all",
		"output": "full", "limit": 200,
	})
}

func BenchmarkRealMCPLiteralSnippetsText(b *testing.B) {
	benchmarkRealMCP(b, map[string]any{
		"query": "sin3", "mode": "literal", "scope": "text",
		"output": "snippets", "limit": 200,
	})
}

func BenchmarkRealMCPRegexPMIDsAll(b *testing.B) {
	benchmarkRealMCP(b, map[string]any{
		"query": "sin3", "mode": "regex", "scope": "all",
		"output": "pmids", "limit": 200,
	})
}

func BenchmarkRealMCPRankedFull(b *testing.B) {
	benchmarkRealMCP(b, map[string]any{
		"query": "sin3", "mode": "ranked", "scope": "all",
		"output": "full", "limit": 200,
	})
}
