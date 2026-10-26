// Package mcpserver exposes the corpus through the official Go MCP SDK.
package mcpserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/canninbot/pdfmcp/internal/config"
	"github.com/canninbot/pdfmcp/internal/corpus"
	indexdb "github.com/canninbot/pdfmcp/internal/index"
	"github.com/canninbot/pdfmcp/internal/mineru"
	"github.com/canninbot/pdfmcp/internal/renderer"
	"github.com/canninbot/pdfmcp/internal/search"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	version              = "0.1.4"
	maxToolJSONBytes     = 4 << 20
	maxResourceBytes     = 100 << 20
	maxInlinePDFBytes    = 1 << 20
	defaultPDFChunkBytes = 256 << 10
)

// Service owns the read-only index, search engine, and lazy renderer.
type Service struct {
	config    config.Config
	index     *indexdb.DB
	search    *search.Engine
	renderer  *renderer.Renderer
	server    *mcp.Server
	publicURL *url.URL
}

// Options controls transport-aware metadata advertised by the MCP service.
type Options struct {
	PublicURL string
}

type searchArgs struct {
	Query           string                  `json:"query,omitempty" jsonschema:"search expression; optional when metadata_filters are supplied"`
	Mode            string                  `json:"mode,omitempty" jsonschema:"ranked, literal, or regex; default ranked"`
	Scope           string                  `json:"scope,omitempty" jsonschema:"text, figures, tables, metadata, or all; default text"`
	PMIDs           []string                `json:"pmids,omitempty" jsonschema:"optional PMID allowlist"`
	CaseSensitive   bool                    `json:"case_sensitive,omitempty" jsonschema:"case-sensitive literal or regex matching"`
	ContextLines    int                     `json:"context_lines,omitempty" jsonschema:"surrounding lines for exact modes, maximum 20"`
	Limit           int                     `json:"limit,omitempty" jsonschema:"result limit, default 20; maximum 5000 for pmids and 200 otherwise"`
	Offset          int                     `json:"offset,omitempty" jsonschema:"result offset"`
	Output          string                  `json:"output,omitempty" jsonschema:"pmids, snippets, or full; default full"`
	Debug           bool                    `json:"debug,omitempty" jsonschema:"include query-planner diagnostics"`
	MetadataFilters []search.MetadataFilter `json:"metadata_filters,omitempty" jsonschema:"optional numeric predicates for metadata scope"`
}

type pmidArgs struct {
	PMID string `json:"pmid" jsonschema:"required PubMed identifier"`
}

type getDocumentArgs struct {
	PMID     string `json:"pmid" jsonschema:"required PubMed identifier"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"maximum Unicode characters to return"`
	Offset   int    `json:"offset,omitempty" jsonschema:"Unicode character offset"`
}

type getPageArgs struct {
	PMID string `json:"pmid" jsonschema:"required PubMed identifier"`
	Page int    `json:"page" jsonschema:"required one-based page number"`
}

type getFigureArgs struct {
	PMID  string `json:"pmid" jsonschema:"required PubMed identifier"`
	Image string `json:"image" jsonschema:"required extracted image filename"`
}

type listDocumentsArgs struct {
	Limit  int `json:"limit,omitempty" jsonschema:"document limit, default 20 and maximum 200"`
	Offset int `json:"offset,omitempty" jsonschema:"document offset"`
}

type renderRegionArgs struct {
	PMID         string     `json:"pmid" jsonschema:"required PubMed identifier"`
	Page         int        `json:"page" jsonschema:"required one-based page number"`
	Region       *[]float64 `json:"region,omitempty" jsonschema:"optional normalized x1,y1,x2,y2 rectangle"`
	BlockID      string     `json:"block_id,omitempty" jsonschema:"optional MinerU block identifier"`
	DPI          int        `json:"dpi,omitempty" jsonschema:"render DPI, default 140 and maximum 600"`
	MaxDimension int        `json:"max_dimension,omitempty" jsonschema:"maximum output dimension, default 1568"`
}

// New opens the index read-only and registers exactly eight tools.
//
// Args:
//   - ctx: Context used while opening the index.
//   - cfg: Corpus and database configuration.
//   - options: Optional externally reachable URL metadata.
//
// Returns:
//   - The configured service, or a validation/open error.
func New(ctx context.Context, cfg config.Config, options Options) (*Service, error) {
	publicURL, err := parsePublicURL(options.PublicURL)
	if err != nil {
		return nil, err
	}
	database, err := indexdb.Open(ctx, cfg, true)
	if err != nil {
		return nil, err
	}
	service := &Service{
		config: cfg, index: database, search: search.New(database), renderer: renderer.New(),
		publicURL: publicURL,
	}
	instructions := "Search and retrieve the local read-only MinerU scientific PDF corpus. Large PDF resources return a manifest with chunk URIs."
	if publicURL != nil {
		instructions = fmt.Sprintf("For original PDFs, prefer one HTTP GET to %s?download=PMID when the client can fetch URLs and write files; do not reconstruct PDFs from MCP chunks unless HTTP is unavailable. get_document_info and large-PDF manifests provide an exact download_url. %s", publicURL.String(), instructions)
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name: "pdfmcp", Version: version,
	}, &mcp.ServerOptions{Instructions: instructions})
	service.server = server
	service.registerTools()
	service.registerResources()
	return service, nil
}

// Server returns the configured SDK server for stdio or in-memory tests.
func (s *Service) Server() *mcp.Server {
	return s.server
}

// parsePublicURL validates an optional externally reachable MCP endpoint.
//
// Args:
//   - value: Absolute HTTP(S) endpoint, or an empty string.
//
// Returns:
//   - The parsed URL, or a validation error.
func parsePublicURL(value string) (*url.URL, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("public URL must be an absolute HTTP(S) URL, got %q", value)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("public URL must use http or https, got %q", parsed.Scheme)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("public URL must not contain credentials, a query, or a fragment")
	}
	return parsed, nil
}

// pdfDownloadAdvertisement describes the preferred original-PDF delivery path.
//
// Args:
//   - pmid: Validated PubMed identifier.
//
// Returns:
//   - Structured metadata suitable for MCP tool and resource responses.
func (s *Service) pdfDownloadAdvertisement(pmid string) map[string]any {
	query := "?download=" + url.QueryEscape(pmid)
	advertisement := map[string]any{
		"method":             "GET",
		"download_url":       query,
		"download_url_kind":  "relative_to_mcp_endpoint",
		"preferred_delivery": "mcp_chunks",
		"query":              query,
		"instruction":        "Resolve download_url against the Streamable HTTP MCP endpoint when available; otherwise use MCP chunks.",
	}
	if s.publicURL == nil {
		advertisement["endpoint"] = "the configured Streamable HTTP MCP endpoint"
		return advertisement
	}
	target := *s.publicURL
	values := target.Query()
	values.Set("download", pmid)
	target.RawQuery = values.Encode()
	advertisement["endpoint"] = s.publicURL.String()
	advertisement["download_url"] = target.String()
	advertisement["download_url_kind"] = "absolute"
	advertisement["preferred_delivery"] = "direct_http"
	advertisement["instruction"] = "When HTTP and filesystem access are available, fetch download_url directly instead of reading paper:// PDF chunks."
	return advertisement
}

// PDFDownloadHandler streams an original PDF without base64 expansion.
//
// The request must use GET or HEAD and include exactly one download=PMID query
// parameter. Paths are resolved through the corpus store and remain confined to
// the configured corpus root.
func (s *Service) PDFDownloadHandler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := request.URL.Query()
		pmids, ok := query["download"]
		if !ok || len(query) != 1 || len(pmids) != 1 {
			http.Error(response, "exactly one download PMID is required", http.StatusBadRequest)
			return
		}
		pmid := pmids[0]
		if err := corpus.ValidatePMID(pmid); err != nil {
			http.NotFound(response, request)
			return
		}
		pdfPath, err := s.index.Store().PDF(pmid)
		if err != nil {
			http.NotFound(response, request)
			return
		}
		file, err := os.Open(pdfPath)
		if err != nil {
			http.NotFound(response, request)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.Size() > maxResourceBytes {
			http.Error(response, "PDF unavailable or exceeds 100 MiB limit", http.StatusRequestEntityTooLarge)
			return
		}
		response.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", pmid+".pdf"))
		response.Header().Set("Content-Type", "application/pdf")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(response, request, pmid+".pdf", info.ModTime(), file)
	})
}

// Close releases the renderer and SQLite connections.
func (s *Service) Close() error {
	renderErr := s.renderer.Close()
	indexErr := s.index.Close()
	return errors.Join(renderErr, indexErr)
}

func (s *Service) registerTools() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "search", Description: "Search indexed paper content or optional metadata using ranked FTS5, literal substring, or RE2 regex mode. Metadata responses warn for every field below 75% completeness except pmcid. Sparse fields such as study_designs cannot produce exhaustive cohorts: a Clinical Trial query finds only records with populated matching study_designs, and missing values do not mean a paper is not a clinical trial.",
	}, s.searchTool)
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "get_document", Description: "Read a bounded page of one paper's extracted Markdown.",
	}, s.getDocumentTool)
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "get_page", Description: "Retrieve MinerU extracted blocks for one one-based PDF page.",
	}, s.getPageTool)
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "get_figure", Description: "Retrieve an existing MinerU PNG or JPEG with its caption and provenance.",
	}, s.getFigureTool)
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "list_documents", Description: "Browse indexed papers in stable PMID order.",
	}, s.listDocumentsTool)
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "get_document_info", Description: "Return metadata, source inventory, outline, figures, tables, resource URIs, and the preferred direct PDF download URL.",
	}, s.getDocumentInfoTool)
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "render_region", Description: "Render an original PDF page or MinerU block crop with embedded PDFium WASM.",
	}, s.renderRegionTool)
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "corpus_stats", Description: "Return aggregate index and corpus statistics without searching.",
	}, s.corpusStatsTool)
}

func (s *Service) registerResources() {
	handler := s.readResource
	for _, template := range []*mcp.ResourceTemplate{
		{Name: "Paper Markdown", URITemplate: "paper://{pmid}/markdown", MIMEType: "text/markdown"},
		{Name: "Structured MinerU JSON", URITemplate: "paper://{pmid}/structured", MIMEType: "application/json"},
		{Name: "Middle MinerU JSON", URITemplate: "paper://{pmid}/middle", MIMEType: "application/json"},
		{Name: "Metadata annotations", URITemplate: "paper://{pmid}/annotations", MIMEType: "application/json"},
		{Name: "Original PDF", URITemplate: "paper://{pmid}/pdf", Description: "For saving a PDF, prefer the download_url from get_document_info. This resource is inline up to 1 MiB; larger files return a JSON download and chunk manifest."},
		{Name: "Original PDF chunk", URITemplate: "paper://{pmid}/pdf{?offset,length}", MIMEType: "application/octet-stream", Description: "Bounded byte range from an original PDF; maximum length 256 KiB."},
		{Name: "Extracted image", URITemplate: "paper://{pmid}/image/{image}", MIMEType: "image/*"},
	} {
		s.server.AddResourceTemplate(template, handler)
	}
}

func (s *Service) searchTool(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	response, err := s.search.Search(ctx, search.Options{
		Query: args.Query, Mode: args.Mode, Scope: args.Scope, PMIDs: args.PMIDs,
		CaseSensitive: args.CaseSensitive, ContextLines: args.ContextLines,
		Limit: args.Limit, Offset: args.Offset, Output: args.Output, Debug: args.Debug,
		MetadataFilters: args.MetadataFilters,
	})
	if err != nil {
		return nil, nil, err
	}
	if response.Output == "pmids" {
		return jsonToolResultLimited(response, config.MaxPMIDResponseBytes, "PMID search")
	}
	return jsonToolResult(response, nil)
}

func (s *Service) getDocumentTool(_ context.Context, _ *mcp.CallToolRequest, args getDocumentArgs) (*mcp.CallToolResult, any, error) {
	if err := corpus.ValidatePMID(args.PMID); err != nil {
		return nil, nil, err
	}
	if args.Offset < 0 {
		return nil, nil, errors.New("offset cannot be negative")
	}
	if args.MaxChars == 0 {
		args.MaxChars = 50_000
	}
	if args.MaxChars < 1 || args.MaxChars > s.config.TextLimit {
		return nil, nil, fmt.Errorf("max_chars must be between 1 and %d", s.config.TextLimit)
	}
	path, err := s.index.Store().ResolveSource(args.PMID, "markdown.md")
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	runes := []rune(string(data))
	if args.Offset > len(runes) {
		return nil, nil, fmt.Errorf("offset %d exceeds document length %d", args.Offset, len(runes))
	}
	end := min(len(runes), args.Offset+args.MaxChars)
	result := map[string]any{
		"pmid": args.PMID, "text": string(runes[args.Offset:end]), "offset": args.Offset,
		"returned_chars": end - args.Offset, "total_chars": len(runes), "has_more": end < len(runes),
		"source_file": "markdown.md", "resource_uri": "paper://" + args.PMID + "/markdown",
	}
	return jsonToolResult(result, nil)
}

func (s *Service) getPageTool(ctx context.Context, _ *mcp.CallToolRequest, args getPageArgs) (*mcp.CallToolResult, any, error) {
	if err := corpus.ValidatePMID(args.PMID); err != nil {
		return nil, nil, err
	}
	if args.Page < 1 {
		return nil, nil, errors.New("page must be one-based and positive")
	}
	blocks, err := s.index.PageBlocks(ctx, args.PMID, args.Page-1)
	if err != nil {
		return nil, nil, err
	}
	if len(blocks) == 0 {
		return nil, nil, fmt.Errorf("page-specific MinerU content is unavailable for PMID %s page %d", args.PMID, args.Page)
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Searchable != "" {
			parts = append(parts, block.Searchable)
		}
	}
	result := map[string]any{
		"pmid": args.PMID, "page": args.Page, "text": strings.Join(parts, "\n\n"),
		"blocks": blocks, "source": "MinerU structured content",
		"resource_uri": "paper://" + args.PMID + "/structured",
	}
	return jsonToolResult(result, nil)
}

func (s *Service) getFigureTool(ctx context.Context, _ *mcp.CallToolRequest, args getFigureArgs) (*mcp.CallToolResult, any, error) {
	if err := corpus.ValidatePMID(args.PMID); err != nil {
		return nil, nil, err
	}
	path, err := s.index.Store().ResolveImage(args.PMID, args.Image)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > s.config.ImageLimit {
		return nil, nil, fmt.Errorf("image exceeds %d-byte limit", s.config.ImageLimit)
	}
	mimeType := corpus.MIMEType(path)
	if mimeType != "image/png" && mimeType != "image/jpeg" {
		return nil, nil, fmt.Errorf("unsupported image MIME type %s", mimeType)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	metadata := map[string]any{
		"pmid": args.PMID, "image": args.Image, "mime_type": mimeType,
		"size": info.Size(), "resource_uri": "paper://" + args.PMID + "/image/" + args.Image,
	}
	blocks, _ := s.index.AllBlocks(ctx, args.PMID)
	for _, block := range blocks {
		if filepath.Base(block.ImagePath) == args.Image {
			metadata["caption"] = block.Caption
			metadata["block_id"] = block.BlockID
			if block.PageIndex != nil {
				metadata["page"] = *block.PageIndex + 1
			}
			break
		}
	}
	encoded, _ := json.Marshal(metadata)
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: string(encoded)},
		&mcp.ImageContent{Data: data, MIMEType: mimeType},
	}}, metadata, nil
}

func (s *Service) listDocumentsTool(ctx context.Context, _ *mcp.CallToolRequest, args listDocumentsArgs) (*mcp.CallToolResult, any, error) {
	if args.Limit == 0 {
		args.Limit = config.DefaultQueryLimit
	}
	if args.Limit < 1 || args.Limit > config.MaxQueryLimit || args.Offset < 0 {
		return nil, nil, errors.New("limit must be 1-200 and offset cannot be negative")
	}
	documents, err := s.index.ListDocuments(ctx, args.Limit, args.Offset)
	return jsonToolResult(map[string]any{
		"limit": args.Limit, "offset": args.Offset, "documents": documents,
	}, err)
}

func (s *Service) getDocumentInfoTool(ctx context.Context, _ *mcp.CallToolRequest, args pmidArgs) (*mcp.CallToolResult, any, error) {
	if err := corpus.ValidatePMID(args.PMID); err != nil {
		return nil, nil, err
	}
	document, err := s.index.GetDocument(ctx, args.PMID)
	if err != nil {
		return nil, nil, err
	}
	files, err := s.index.Store().Inventory(args.PMID)
	if err != nil {
		return nil, nil, err
	}
	blocks, err := s.index.AllBlocks(ctx, args.PMID)
	if err != nil {
		return nil, nil, err
	}
	sections := make([]string, 0)
	figures := make([]indexdb.Block, 0)
	tables := make([]indexdb.Block, 0)
	seenSections := make(map[string]struct{})
	for _, block := range blocks {
		if block.Section != "" {
			if _, seen := seenSections[block.Section]; !seen {
				sections = append(sections, block.Section)
				seenSections[block.Section] = struct{}{}
			}
		}
		switch block.Type {
		case "figure":
			figures = append(figures, block)
		case "table":
			tables = append(tables, block)
		}
	}
	resources := make([]string, 0)
	var annotation *indexdb.Annotation
	if value, annotationErr := s.index.GetAnnotation(ctx, args.PMID); annotationErr == nil {
		annotation = &value
		resources = append(resources, "paper://"+args.PMID+"/annotations")
	} else if !errors.Is(annotationErr, sql.ErrNoRows) {
		return nil, nil, annotationErr
	}
	pdfDownload := map[string]any{"available": false}
	for _, file := range files {
		switch file.Path {
		case "markdown.md":
			resources = append(resources, "paper://"+args.PMID+"/markdown")
		case "structured_content.json":
			resources = append(resources, "paper://"+args.PMID+"/structured")
		case "middle_json.json":
			resources = append(resources, "paper://"+args.PMID+"/middle")
		default:
			if strings.HasPrefix(file.Path, "images/") {
				resources = append(resources, "paper://"+args.PMID+"/image/"+filepath.Base(file.Path))
			} else if strings.HasSuffix(strings.ToLower(file.Path), ".pdf") {
				resources = append(resources, "paper://"+args.PMID+"/pdf")
				pdfDownload = s.pdfDownloadAdvertisement(args.PMID)
				pdfDownload["available"] = true
				pdfDownload["size_bytes"] = file.Size
			}
		}
	}
	response := map[string]any{
		"document": document, "source_files": files, "section_outline": sections,
		"figures": figures, "tables": tables, "resource_uris": resources,
		"http_pdf_download": pdfDownload,
	}
	if annotation != nil {
		response["metadata_annotation"] = annotation
	}
	return jsonToolResult(response, nil)
}

func (s *Service) renderRegionTool(ctx context.Context, _ *mcp.CallToolRequest, args renderRegionArgs) (*mcp.CallToolResult, any, error) {
	if err := corpus.ValidatePMID(args.PMID); err != nil {
		return nil, nil, err
	}
	if args.Region != nil && args.BlockID != "" {
		return nil, nil, errors.New("region and block_id conflict; supply only one")
	}
	if args.DPI == 0 {
		args.DPI = 140
	}
	if args.MaxDimension == 0 {
		args.MaxDimension = 1568
	}
	var region *renderer.Region
	if args.Region != nil {
		if len(*args.Region) != 4 {
			return nil, nil, errors.New("region must contain exactly four coordinates")
		}
		value := renderer.Region{(*args.Region)[0], (*args.Region)[1], (*args.Region)[2], (*args.Region)[3]}
		region = &value
	}
	if args.BlockID != "" {
		block, err := s.index.FindBlock(ctx, args.PMID, args.BlockID)
		if err != nil {
			return nil, nil, err
		}
		if block.PageIndex == nil || *block.PageIndex+1 != args.Page {
			return nil, nil, errors.New("block does not belong to the requested page")
		}
		var bbox mineru.BBox
		if len(block.BBox) == 0 || json.Unmarshal(block.BBox, &bbox) != nil || bbox.Normalized == nil {
			return nil, nil, errors.New("block has no known normalized bounding box")
		}
		value := renderer.Region(*bbox.Normalized)
		region = &value
	}
	pdfPath, err := s.index.Store().PDF(args.PMID)
	if err != nil {
		return nil, nil, err
	}
	output, err := s.renderer.Render(ctx, pdfPath, args.Page, region, args.DPI, args.MaxDimension)
	if err != nil {
		return nil, nil, err
	}
	metadata := map[string]any{
		"pmid": args.PMID, "page": args.Page, "width": output.Width,
		"height": output.Height, "effective_dpi": output.EffectiveDPI,
		"cropped": output.Cropped, "source": filepath.Base(pdfPath),
	}
	encoded, _ := json.Marshal(metadata)
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: string(encoded)},
		&mcp.ImageContent{Data: output.PNG, MIMEType: "image/png"},
	}}, metadata, nil
}

func (s *Service) corpusStatsTool(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	stats, err := s.index.CorpusStats(ctx)
	return jsonToolResult(stats, err)
}

func (s *Service) readResource(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	parsed, err := url.Parse(request.Params.URI)
	if err != nil || parsed.Scheme != "paper" {
		return nil, mcp.ResourceNotFoundError(request.Params.URI)
	}
	pmid := parsed.Host
	if err := corpus.ValidatePMID(pmid); err != nil {
		return nil, mcp.ResourceNotFoundError(request.Params.URI)
	}
	components := strings.Split(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/")
	for position := range components {
		components[position], err = url.PathUnescape(components[position])
		if err != nil {
			return nil, mcp.ResourceNotFoundError(request.Params.URI)
		}
	}
	var path string
	switch {
	case len(components) == 1 && components[0] == "annotations":
		annotation, annotationErr := s.index.GetAnnotation(ctx, pmid)
		if annotationErr != nil {
			return nil, mcp.ResourceNotFoundError(request.Params.URI)
		}
		encoded, annotationErr := json.Marshal(annotation)
		if annotationErr != nil {
			return nil, annotationErr
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: request.Params.URI, MIMEType: "application/json", Text: string(encoded),
		}}}, nil
	case len(components) == 1 && components[0] == "markdown":
		path, err = s.index.Store().ResolveSource(pmid, "markdown.md")
	case len(components) == 1 && components[0] == "structured":
		path, err = s.index.Store().ResolveSource(pmid, "structured_content.json")
	case len(components) == 1 && components[0] == "middle":
		path, err = s.index.Store().ResolveSource(pmid, "middle_json.json")
	case len(components) == 1 && components[0] == "pdf":
		path, err = s.index.Store().PDF(pmid)
	case len(components) == 2 && components[0] == "image":
		path, err = s.index.Store().ResolveImage(pmid, components[1])
	default:
		err = os.ErrNotExist
	}
	if err != nil {
		return nil, mcp.ResourceNotFoundError(request.Params.URI)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxResourceBytes {
		return nil, fmt.Errorf("resource unavailable or exceeds 100 MiB limit")
	}
	if len(components) == 1 && components[0] == "pdf" {
		return s.readPDFResource(request.Params.URI, parsed, path, info.Size())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxResourceBytes+1))
	if err != nil {
		return nil, err
	}
	mimeType := corpus.MIMEType(path)
	content := &mcp.ResourceContents{URI: request.Params.URI, MIMEType: mimeType}
	if mimeType == "text/markdown" || mimeType == "application/json" {
		content.Text = strings.ToValidUTF8(string(data), "�")
	} else {
		content.Blob = data
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{content}}, nil
}

// readPDFResource returns a small PDF inline, a large-PDF manifest, or a
// requested bounded byte range.
//
// Args:
//   - uri: Original MCP resource URI.
//   - parsed: Parsed form of uri.
//   - path: Safely resolved PDF path.
//   - size: PDF size in bytes.
//
// Returns:
//   - The MCP resource response, or a validation/read error.
func (s *Service) readPDFResource(uri string, parsed *url.URL, path string, size int64) (*mcp.ReadResourceResult, error) {
	offset, length, ranged, err := parsePDFRange(parsed, size)
	if err != nil {
		return nil, err
	}
	if ranged {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		data := make([]byte, length)
		if _, err := file.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		content := &mcp.ResourceContents{
			URI: uri, MIMEType: "application/octet-stream", Blob: data,
			Meta: mcp.Meta{
				"offset": offset, "returned_bytes": length, "total_bytes": size,
				"has_more": offset+length < size,
			},
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{content}}, nil
	}
	if size > maxInlinePDFBytes {
		digest, err := fileSHA256(path)
		if err != nil {
			return nil, err
		}
		download := s.pdfDownloadAdvertisement(parsed.Host)
		manifest := map[string]any{
			"pmid":                       parsed.Host,
			"mime_type":                  "application/pdf",
			"size_bytes":                 size,
			"sha256":                     digest,
			"download_url":               download["download_url"],
			"download_url_kind":          download["download_url_kind"],
			"preferred_delivery":         download["preferred_delivery"],
			"delivery_instruction":       download["instruction"],
			"http_download_method":       download["method"],
			"http_download_endpoint":     download["endpoint"],
			"http_download_query":        download["query"],
			"chunk_uri_template":         "paper://" + parsed.Host + "/pdf?offset={offset}&length={length}",
			"default_chunk_bytes":        defaultPDFChunkBytes,
			"maximum_chunk_bytes":        defaultPDFChunkBytes,
			"default_chunk_count":        (size + defaultPDFChunkBytes - 1) / defaultPDFChunkBytes,
			"reconstruction_instruction": "Concatenate decoded chunk blobs in ascending offset order and verify SHA-256.",
		}
		encoded, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		content := &mcp.ResourceContents{URI: uri, MIMEType: "application/json", Text: string(encoded)}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{content}}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := &mcp.ResourceContents{URI: uri, MIMEType: "application/pdf", Blob: data}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{content}}, nil
}

// parsePDFRange validates an optional offset and length query.
//
// Args:
//   - parsed: Parsed PDF resource URI.
//   - size: Total PDF size in bytes.
//
// Returns:
//   - Offset, effective length, whether a range was requested, and an error.
func parsePDFRange(parsed *url.URL, size int64) (int64, int64, bool, error) {
	query := parsed.Query()
	if len(query) == 0 {
		return 0, 0, false, nil
	}
	if len(query) != 2 || len(query["offset"]) != 1 || len(query["length"]) != 1 {
		return 0, 0, false, errors.New("PDF chunks require exactly one offset and length")
	}
	offset, err := strconv.ParseInt(query.Get("offset"), 10, 64)
	if err != nil || offset < 0 || offset >= size {
		return 0, 0, false, fmt.Errorf("PDF chunk offset must be between 0 and %d", max(0, size-1))
	}
	length, err := strconv.ParseInt(query.Get("length"), 10, 64)
	if err != nil || length < 1 || length > defaultPDFChunkBytes {
		return 0, 0, false, fmt.Errorf("PDF chunk length must be between 1 and %d", defaultPDFChunkBytes)
	}
	length = min(length, size-offset)
	return offset, length, true, nil
}

// fileSHA256 calculates the lowercase SHA-256 digest of a file.
//
// Args:
//   - path: File to hash.
//
// Returns:
//   - Hexadecimal digest, or a file read error.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func jsonToolResult(value any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	return jsonToolResultLimited(value, maxToolJSONBytes, "tool")
}

func jsonToolResultLimited(value any, maxBytes int, label string) (*mcp.CallToolResult, any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	if !utf8.Valid(encoded) {
		return nil, nil, errors.New("tool result is not valid UTF-8")
	}
	if len(encoded) > maxBytes {
		return nil, nil, fmt.Errorf("%s result exceeds the %d-byte output limit", label, maxBytes)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, value, nil
}

// ToolNames returns the exact advertised tool list for verification tests.
func ToolNames() []string {
	names := []string{
		"search", "get_document", "get_page", "get_figure", "list_documents",
		"get_document_info", "render_region", "corpus_stats",
	}
	slices.Sort(names)
	return names
}
