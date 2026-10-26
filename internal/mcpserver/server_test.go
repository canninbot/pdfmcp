package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	indexdb "github.com/canninbot/pdfmcp/internal/index"
	"github.com/canninbot/pdfmcp/internal/search"
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
	service, err := New(ctx, cfg, Options{})
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
	searchDescription := ""
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Name == "search" {
			searchDescription = tool.Description
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, ToolNames()) {
		t.Fatalf("advertised tools differ: %v", names)
	}
	if !strings.Contains(searchDescription, "75% completeness") || !strings.Contains(searchDescription, "study_designs") {
		t.Fatalf("search description omitted missingness guidance: %q", searchDescription)
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

func TestOptionalMetadataAnnotationInfoAndResource(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := indexdb.Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	annotations := filepath.Join(t.TempDir(), "annotations.csv")
	testutil.WriteAnnotationsCSV(t, annotations)
	if _, err := database.ImportAnnotations(ctx, annotations); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := New(ctx, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	_, structured, err := service.getDocumentInfoTool(ctx, nil, pmidArgs{PMID: "12345678"})
	if err != nil {
		t.Fatal(err)
	}
	info := structured.(map[string]any)
	if info["metadata_annotation"] == nil {
		t.Fatalf("document info omitted annotation: %+v", info)
	}
	resource, err := service.readResource(ctx, &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "paper://12345678/annotations"},
	})
	if err != nil || len(resource.Contents) != 1 {
		t.Fatalf("annotation resource: %+v, %v", resource, err)
	}
	var annotation indexdb.Annotation
	if err := json.Unmarshal([]byte(resource.Contents[0].Text), &annotation); err != nil {
		t.Fatal(err)
	}
	if annotation.Source != "annotations.csv" || !bytes.Contains(annotation.Data, []byte(`"journal_name":"Synthetic Oncology"`)) {
		t.Fatalf("unexpected annotation resource: %+v", annotation)
	}
	_, filtered, err := service.searchTool(ctx, nil, searchArgs{
		Scope: "metadata", Output: "pmids",
		MetadataFilters: []search.MetadataFilter{{Field: "cited_by_count", Operator: ">", Value: 100}},
	})
	if err != nil || !slices.Equal(filtered.(search.Response).PMIDs, []string{"12345678"}) {
		t.Fatalf("MCP metadata filter: %+v, %v", filtered, err)
	}

	_, unannotated, err := service.getDocumentInfoTool(ctx, nil, pmidArgs{PMID: "23456789"})
	if err != nil {
		t.Fatal(err)
	}
	if unannotated.(map[string]any)["metadata_annotation"] == nil {
		t.Fatal("second imported annotation was omitted")
	}
}

func TestPublicURLValidationAndRelativeFallback(t *testing.T) {
	for _, value := range []string{
		"pdf.example.com/mcp",
		"ftp://pdf.example.com/mcp",
		"https://user:secret@pdf.example.com/mcp",
		"https://pdf.example.com/mcp?token=secret",
		"https://pdf.example.com/mcp#fragment",
	} {
		if _, err := parsePublicURL(value); err == nil {
			t.Errorf("parsePublicURL(%q) succeeded", value)
		}
	}
	parsed, err := parsePublicURL("https://pdf.example.com/mcp")
	if err != nil || parsed.String() != "https://pdf.example.com/mcp" {
		t.Fatalf("valid public URL: %v, %v", parsed, err)
	}
	advertisement := (&Service{}).pdfDownloadAdvertisement("12345678")
	if advertisement["download_url"] != "?download=12345678" ||
		advertisement["download_url_kind"] != "relative_to_mcp_endpoint" {
		t.Fatalf("unexpected relative advertisement: %+v", advertisement)
	}
}

func TestLargePDFResourceManifestAndChunks(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	pdfPath := filepath.Join(cfg.Root, "12345678", "12345678.pdf")
	file, err := os.OpenFile(pdfPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	padding := bytes.Repeat([]byte("large-pdf-test"), maxInlinePDFBytes/14+1)
	if _, err := file.Write(padding); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	wantPDF, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
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
	service, err := New(ctx, cfg, Options{PublicURL: "https://pdf.example.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	_, structured, err := service.getDocumentInfoTool(ctx, nil, pmidArgs{PMID: "12345678"})
	if err != nil {
		t.Fatal(err)
	}
	info := structured.(map[string]any)
	if _, exists := info["metadata_annotation"]; exists {
		t.Fatalf("default document info included absent optional metadata: %+v", info)
	}
	download := info["http_pdf_download"].(map[string]any)
	if download["download_url"] != "https://pdf.example.com/mcp?download=12345678" ||
		download["preferred_delivery"] != "direct_http" || download["available"] != true {
		t.Fatalf("unexpected document download advertisement: %+v", download)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := service.Server().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "pdf-chunk-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	manifestResource, err := clientSession.ReadResource(ctx, &mcp.ReadResourceParams{URI: "paper://12345678/pdf"})
	if err != nil || len(manifestResource.Contents) != 1 {
		t.Fatalf("large PDF manifest: %+v, %v", manifestResource, err)
	}
	var manifest struct {
		SizeBytes         int64  `json:"size_bytes"`
		SHA256            string `json:"sha256"`
		DownloadURL       string `json:"download_url"`
		DownloadURLKind   string `json:"download_url_kind"`
		PreferredDelivery string `json:"preferred_delivery"`
	}
	if err := json.Unmarshal([]byte(manifestResource.Contents[0].Text), &manifest); err != nil {
		t.Fatal(err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(wantPDF))
	if manifest.SizeBytes != int64(len(wantPDF)) || manifest.SHA256 != wantDigest ||
		manifest.DownloadURL != "https://pdf.example.com/mcp?download=12345678" ||
		manifest.DownloadURLKind != "absolute" || manifest.PreferredDelivery != "direct_http" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	var reconstructed []byte
	for offset := int64(0); offset < int64(len(wantPDF)); offset += defaultPDFChunkBytes {
		uri := fmt.Sprintf("paper://12345678/pdf?offset=%d&length=%d", offset, defaultPDFChunkBytes)
		chunk, err := clientSession.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil || len(chunk.Contents) != 1 {
			t.Fatalf("PDF chunk at %d: %+v, %v", offset, chunk, err)
		}
		reconstructed = append(reconstructed, chunk.Contents[0].Blob...)
	}
	if !bytes.Equal(reconstructed, wantPDF) {
		t.Fatalf("reconstructed PDF mismatch: got %d bytes, want %d", len(reconstructed), len(wantPDF))
	}
}
