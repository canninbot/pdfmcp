package mcpserver

import (
	"context"
	"slices"
	"strings"
	"testing"

	indexdb "github.com/canninbot/pdfmcp/internal/index"
	"github.com/canninbot/pdfmcp/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolDiscoveryInvocationAndResources(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := indexdb.Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := service.Server().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "pdfmcp-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, ToolNames()) {
		t.Fatalf("advertised tools differ: %v", names)
	}
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "search", Arguments: map[string]any{
			"query": "TEAD inhibition", "mode": "ranked", "scope": "text",
		},
	})
	if err != nil || result.IsError || len(result.Content) == 0 {
		t.Fatalf("search invocation: %+v, %v", result, err)
	}
	figure, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_figure", Arguments: map[string]any{
			"pmid": "12345678", "image": "page_1_image_0.png",
		},
	})
	if err != nil || figure.IsError || len(figure.Content) != 2 {
		t.Fatalf("figure invocation: %+v, %v", figure, err)
	}
	resource, err := clientSession.ReadResource(ctx, &mcp.ReadResourceParams{URI: "paper://12345678/pdf"})
	if err != nil || len(resource.Contents) != 1 || len(resource.Contents[0].Blob) == 0 {
		t.Fatalf("PDF resource: %+v, %v", resource, err)
	}
	for _, invocation := range []struct {
		name      string
		arguments map[string]any
	}{
		{name: "get_document", arguments: map[string]any{"pmid": "12345678", "max_chars": 100}},
		{name: "get_page", arguments: map[string]any{"pmid": "12345678", "page": 1}},
		{name: "list_documents", arguments: map[string]any{"limit": 1}},
		{name: "get_document_info", arguments: map[string]any{"pmid": "12345678"}},
		{name: "corpus_stats", arguments: map[string]any{}},
	} {
		called, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: invocation.name, Arguments: invocation.arguments,
		})
		if err != nil || called.IsError || len(called.Content) == 0 {
			t.Fatalf("%s invocation: %+v, %v", invocation.name, called, err)
		}
	}
	rendered, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "render_region", Arguments: map[string]any{
			"pmid": "12345678", "page": 1,
			"region": []any{0.25, 0.25, 0.75, 0.75}, "dpi": 72, "max_dimension": 256,
		},
	})
	if err != nil || rendered.IsError || len(rendered.Content) != 2 {
		t.Fatalf("render invocation: %+v, %v", rendered, err)
	}
}

func TestDedicatedPMIDResponseLimit(t *testing.T) {
	value := map[string]string{"pmids": strings.Repeat("1", 128)}
	if _, _, err := jsonToolResultLimited(value, 32, "PMID search"); err == nil || !strings.Contains(err.Error(), "PMID search") {
		t.Fatalf("dedicated response limit was not enforced: %v", err)
	}
}
