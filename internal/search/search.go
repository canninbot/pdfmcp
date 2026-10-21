// Package search implements ranked FTS5 and exact native-Go corpus search.
package search

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/canninbot/pdfmcp/internal/config"
	"github.com/canninbot/pdfmcp/internal/corpus"
	indexdb "github.com/canninbot/pdfmcp/internal/index"
)

const maxSnippetRunes = 2_000
const defaultSearchTimeout = 30 * time.Second
const maxPooledMarkdownBytes = 2 << 20

// Options is the shared CLI and MCP search request.
type Options struct {
	Query         string   `json:"query"`
	Mode          string   `json:"mode,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	PMIDs         []string `json:"pmids,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
	ContextLines  int      `json:"context_lines,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	Offset        int      `json:"offset,omitempty"`
	Output        string   `json:"output,omitempty"`
	Debug         bool     `json:"debug,omitempty"`
}

// Result is a source-traceable search hit.
type Result struct {
	PMID        string   `json:"pmid"`
	Title       string   `json:"document_title,omitempty"`
	Page        *int     `json:"page,omitempty"`
	Section     string   `json:"section,omitempty"`
	Snippet     string   `json:"snippet"`
	Line        *int     `json:"line,omitempty"`
	Position    *int     `json:"position,omitempty"`
	Score       *float64 `json:"score,omitempty"`
	BlockID     string   `json:"block_id,omitempty"`
	ContentType string   `json:"content_type"`
	ResourceURI string   `json:"resource_uri"`
}

// Response includes applied pagination values.
type Response struct {
	Query   string   `json:"query"`
	Mode    string   `json:"mode"`
	Scope   string   `json:"scope"`
	Output  string   `json:"output"`
	Limit   int      `json:"limit"`
	Offset  int      `json:"offset"`
	Results []Result `json:"results"`
	PMIDs   []string `json:"pmids,omitempty"`
	Debug   *Debug   `json:"debug,omitempty"`
}

// Debug describes the selected search strategy without logging to stdout.
type Debug struct {
	Strategy string `json:"strategy"`
	Reason   string `json:"reason"`
}

// Engine shares search logic between the CLI and MCP server.
type Engine struct {
	index         *indexdb.DB
	markdownPaths sync.Map
	markdownData  sync.Pool
}

// New returns a search engine using index.
func New(index *indexdb.DB) *Engine {
	return &Engine{index: index}
}

// Search validates options and runs exactly one selected search mode.
func (e *Engine) Search(ctx context.Context, options Options) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultSearchTimeout)
	defer cancel()
	if err := normalizeOptions(&options); err != nil {
		return Response{}, err
	}
	results := make([]Result, 0)
	pmids := make([]string, 0)
	var err error
	switch options.Mode {
	case "ranked":
		if options.Output == "pmids" {
			pmids, err = e.rankedPMIDs(ctx, options)
		} else {
			results, err = e.ranked(ctx, options)
			if options.Output == "snippets" {
				minimizeResults(results)
			}
		}
	case "literal", "regex":
		results, pmids, err = e.exact(ctx, options)
	default:
		err = fmt.Errorf("unsupported mode %q", options.Mode)
	}
	response := Response{
		Query: options.Query, Mode: options.Mode, Scope: options.Scope,
		Output: options.Output, Limit: options.Limit, Offset: options.Offset,
		Results: results, PMIDs: pmids,
	}
	if options.Debug {
		response.Debug = e.strategyDebug(options)
	}
	return response, err
}

func (e *Engine) strategyDebug(options Options) *Debug {
	if options.Mode == "ranked" {
		return &Debug{Strategy: "fts5-bm25", Reason: "ranked mode uses the token index"}
	}
	if options.Scope == "text" {
		if options.Output == "pmids" {
			return &Debug{Strategy: "go-markdown-scan", Reason: "PMID-only text search stops after each document's first match"}
		}
		return &Debug{Strategy: "go-markdown-lines", Reason: "text results require Markdown line references"}
	}
	literal := candidateLiteral(options)
	if _, ok := trigramExpression(literal, options.Scope); ok && e.index.HasTrigramIndex() {
		return &Debug{Strategy: "fts5-trigram-prefilter", Reason: "a required substring narrows documents before exact verification"}
	}
	return &Debug{Strategy: "sqlite-stream-scan", Reason: "no correctness-safe indexed prerequisite is available"}
}

func minimizeResults(results []Result) {
	for position := range results {
		results[position].Title = ""
		results[position].Section = ""
		results[position].Score = nil
	}
}

func (e *Engine) ranked(ctx context.Context, options Options) ([]Result, error) {
	where := []string{"fts_blocks MATCH ?"}
	arguments := []any{options.Query}
	if options.Scope != "all" {
		clause, err := scopeClause(options.Scope)
		if err != nil {
			return nil, err
		}
		where = append(where, clause)
	}
	if len(options.PMIDs) > 0 {
		placeholders := make([]string, len(options.PMIDs))
		for position, pmid := range options.PMIDs {
			placeholders[position] = "?"
			arguments = append(arguments, pmid)
		}
		where = append(where, "b.pmid IN ("+strings.Join(placeholders, ",")+")")
	}
	arguments = append(arguments, options.Limit, options.Offset)
	query := `
		SELECT b.id, b.pmid, b.block_id, b.page_idx, b.block_type,
		       COALESCE(b.section, ''), COALESCE(b.text, ''),
		       COALESCE(b.caption, ''), COALESCE(b.bbox_json, ''),
		       COALESCE(b.image_path, ''), COALESCE(b.source_locator, ''),
		       b.searchable_text, COALESCE(d.title, ''),
		       bm25(fts_blocks, 1.0, 0.25) AS score,
		       snippet(fts_blocks, 0, '<mark>', '</mark>', ' ... ', 40)
		FROM fts_blocks
		JOIN blocks b ON b.id = fts_blocks.rowid
		JOIN documents d ON d.pmid = b.pmid
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY score, b.pmid, b.id LIMIT ? OFFSET ?`
	rows, err := e.index.SQL().QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("ranked FTS query: %w", err)
	}
	defer rows.Close()
	results := make([]Result, 0, options.Limit)
	for rows.Next() {
		var block indexdb.Block
		var page sql.NullInt64
		var bbox, title, snippet string
		var score float64
		if err := rows.Scan(&block.ID, &block.PMID, &block.BlockID, &page,
			&block.Type, &block.Section, &block.Text, &block.Caption, &bbox,
			&block.ImagePath, &block.Source, &block.Searchable, &title, &score,
			&snippet); err != nil {
			return nil, err
		}
		result := Result{
			PMID: block.PMID, Title: title, Section: block.Section,
			Snippet: truncateRunes(snippet, maxSnippetRunes), Score: &score,
			BlockID: block.BlockID, ContentType: block.Type,
			ResourceURI: resourceURI(block.PMID, block.Type, block.ImagePath),
		}
		if page.Valid {
			value := int(page.Int64) + 1
			result.Page = &value
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (e *Engine) rankedPMIDs(ctx context.Context, options Options) ([]string, error) {
	where := []string{"fts_blocks MATCH ?"}
	arguments := []any{options.Query}
	if options.Scope != "all" {
		clause, err := scopeClause(options.Scope)
		if err != nil {
			return nil, err
		}
		where = append(where, clause)
	}
	if len(options.PMIDs) > 0 {
		placeholders := make([]string, len(options.PMIDs))
		for position, pmid := range options.PMIDs {
			placeholders[position] = "?"
			arguments = append(arguments, pmid)
		}
		where = append(where, "b.pmid IN ("+strings.Join(placeholders, ",")+")")
	}
	query := `
		SELECT b.pmid
		FROM fts_blocks JOIN blocks b ON b.id = fts_blocks.rowid
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY bm25(fts_blocks, 1.0, 0.25), b.pmid, b.id`
	rows, err := e.index.SQL().QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("ranked PMID query: %w", err)
	}
	defer rows.Close()
	pmids := make([]string, 0, options.Limit)
	seen := make(map[string]struct{})
	skipped := 0
	for rows.Next() && len(pmids) < options.Limit {
		var pmid string
		if err := rows.Scan(&pmid); err != nil {
			return nil, err
		}
		if _, exists := seen[pmid]; exists {
			continue
		}
		seen[pmid] = struct{}{}
		if skipped < options.Offset {
			skipped++
			continue
		}
		pmids = append(pmids, pmid)
	}
	return pmids, rows.Err()
}

func (e *Engine) exact(ctx context.Context, options Options) ([]Result, []string, error) {
	var regex *regexp.Regexp
	if options.Mode == "regex" {
		pattern := options.Query
		if !options.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid RE2 expression: %w", err)
		}
		regex = compiled
	}
	candidatePMIDs, err := e.exactCandidates(ctx, options)
	if err != nil {
		return nil, nil, err
	}
	if options.Output == "pmids" {
		var pmids []string
		if options.Scope == "text" {
			pmids, err = e.markdownPMIDs(ctx, options, regex, candidatePMIDs)
		} else {
			pmids, err = e.blockPMIDs(ctx, options, regex, candidatePMIDs)
		}
		return []Result{}, pmids, err
	}
	if candidatePMIDs != nil && len(candidatePMIDs) == 0 {
		return []Result{}, []string{}, nil
	}
	if options.Scope != "text" {
		results, searchErr := e.searchCandidateBlocks(ctx, options, regex, candidatePMIDs)
		if options.Output == "snippets" {
			minimizeResults(results)
		}
		return results, []string{}, searchErr
	}
	documents, err := e.candidateDocuments(ctx, candidatePMIDs)
	if err != nil {
		return nil, nil, err
	}
	needed := options.Offset + options.Limit
	all := make([]Result, 0, needed)
	workers := min(32, max(1, runtime.GOMAXPROCS(0)))
	batchSize := max(256, workers*8)
	for start := 0; start < len(documents) && len(all) < needed; start += batchSize {
		end := min(start+batchSize, len(documents))
		batch, searchErr := e.searchMarkdownBatch(
			ctx, documents[start:end], options, regex, workers,
		)
		if searchErr != nil {
			return nil, nil, searchErr
		}
		for _, results := range batch {
			all = append(all, results...)
			if len(all) >= needed {
				break
			}
		}
	}
	if options.Offset >= len(all) {
		return []Result{}, []string{}, nil
	}
	end := min(options.Offset+options.Limit, len(all))
	results := all[options.Offset:end]
	if options.Output == "snippets" {
		minimizeResults(results)
	}
	return results, []string{}, nil
}

func (e *Engine) searchMarkdownBatch(
	ctx context.Context,
	documents []indexdb.Document,
	options Options,
	regex *regexp.Regexp,
	workers int,
) ([][]Result, error) {
	results := make([][]Result, len(documents))
	jobs := make(chan int)
	errChannel := make(chan error, 1)
	var group sync.WaitGroup
	for range min(workers, len(documents)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for position := range jobs {
				if ctx.Err() != nil {
					return
				}
				matches, err := e.searchMarkdown(documents[position], options, regex)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					select {
					case errChannel <- err:
					default:
					}
					return
				}
				results[position] = matches
			}
		}()
	}
	for position := range documents {
		select {
		case jobs <- position:
		case err := <-errChannel:
			close(jobs)
			group.Wait()
			return nil, err
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	group.Wait()
	select {
	case err := <-errChannel:
		return nil, err
	default:
		return results, nil
	}
}

func (e *Engine) exactCandidates(
	ctx context.Context,
	options Options,
) ([]string, error) {
	literal := candidateLiteral(options)
	var candidates []string
	if options.Scope != "text" && e.index.HasTrigramIndex() {
		expression, ok := trigramExpression(literal, options.Scope)
		if ok {
			var err error
			candidates, err = e.trigramCandidates(ctx, expression)
			if err != nil {
				return nil, err
			}
		}
	}
	if candidates == nil {
		if len(options.PMIDs) == 0 {
			return nil, nil
		}
		candidates = append([]string(nil), options.PMIDs...)
		slices.Sort(candidates)
		return slices.Compact(candidates), nil
	}
	if len(options.PMIDs) == 0 {
		return candidates, nil
	}
	allowed := make(map[string]struct{}, len(options.PMIDs))
	for _, pmid := range options.PMIDs {
		allowed[pmid] = struct{}{}
	}
	filtered := candidates[:0]
	for _, pmid := range candidates {
		if _, ok := allowed[pmid]; ok {
			filtered = append(filtered, pmid)
		}
	}
	return filtered, nil
}

func candidateLiteral(options Options) string {
	if options.Mode == "literal" {
		return options.Query
	}
	if regexp.QuoteMeta(options.Query) == options.Query {
		return options.Query
	}
	plannerRegex, err := regexp.Compile(options.Query)
	if err != nil {
		return ""
	}
	literal, _ := plannerRegex.LiteralPrefix()
	if literal == "" {
		literal = safeFTSTerm(options.Query)
	}
	return literal
}

func trigramExpression(literal, scope string) (string, bool) {
	if strings.ContainsRune(literal, 0) {
		return "", false
	}
	runes := []rune(foldForTrigrams(literal))
	if len(runes) < 3 {
		return "", false
	}
	seen := make(map[string]struct{}, len(runes)-2)
	terms := make([]string, 0, len(runes)-2)
	for index := 0; index+3 <= len(runes); index++ {
		term := string(runes[index : index+3])
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	expression := strings.Join(terms, " AND ")
	switch scope {
	case "figures":
		expression = "figures : (" + expression + ")"
	case "tables":
		expression = "tables : (" + expression + ")"
	}
	return expression, true
}

func foldForTrigrams(value string) string {
	return strings.Map(func(original rune) rune {
		canonical := original
		for folded := unicode.SimpleFold(original); folded != original; folded = unicode.SimpleFold(folded) {
			if folded < canonical {
				canonical = folded
			}
		}
		return canonical
	}, value)
}

func (e *Engine) trigramCandidates(ctx context.Context, expression string) ([]string, error) {
	rows, err := e.index.SQL().QueryContext(ctx, `
		SELECT ds.pmid
		FROM fts_documents_trigram AS f
		JOIN document_search AS ds ON ds.id = f.rowid
		WHERE fts_documents_trigram MATCH ?
		ORDER BY ds.pmid`, expression)
	if err != nil {
		return nil, fmt.Errorf("trigram candidate query: %w", err)
	}
	defer rows.Close()
	pmids := make([]string, 0)
	for rows.Next() {
		var pmid string
		if err := rows.Scan(&pmid); err != nil {
			return nil, err
		}
		pmids = append(pmids, pmid)
	}
	return pmids, rows.Err()
}

func (e *Engine) searchCandidateBlocks(
	ctx context.Context,
	options Options,
	regex *regexp.Regexp,
	candidatePMIDs []string,
) ([]Result, error) {
	clause, arguments, err := blockSearchFilter(options.Scope, candidatePMIDs)
	if err != nil {
		return nil, err
	}
	rows, err := e.index.SQL().QueryContext(ctx, `
		SELECT b.pmid, COALESCE(d.title, ''), b.block_id, b.page_idx,
		       b.block_type, COALESCE(b.section, ''),
		       COALESCE(b.image_path, ''), b.searchable_text
		FROM blocks AS b JOIN documents AS d ON d.pmid = b.pmid
		WHERE `+clause+`
		ORDER BY b.pmid, b.id`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("exact block query: %w", err)
	}
	defer rows.Close()
	needed := options.Offset + options.Limit
	all := make([]Result, 0, needed)
	for rows.Next() && len(all) < needed {
		var document indexdb.Document
		var blockID, blockType, section, imagePath, searchable string
		var page sql.NullInt64
		if err := rows.Scan(&document.PMID, &document.Title, &blockID, &page,
			&blockType, &section, &imagePath, &searchable); err != nil {
			return nil, err
		}
		lines := strings.Split(strings.ReplaceAll(searchable, "\r\n", "\n"), "\n")
		matches := matchingLines(document, lines, options, regex, blockType,
			blockID, resourceURI(document.PMID, blockType, imagePath))
		for position := range matches {
			if options.Output == "full" {
				matches[position].Section = section
			}
			if page.Valid {
				value := int(page.Int64) + 1
				matches[position].Page = &value
			}
		}
		all = append(all, matches...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if options.Offset >= len(all) {
		return []Result{}, nil
	}
	return all[options.Offset:min(needed, len(all))], nil
}

func (e *Engine) blockPMIDs(
	ctx context.Context,
	options Options,
	regex *regexp.Regexp,
	candidatePMIDs []string,
) ([]string, error) {
	if candidatePMIDs != nil && len(candidatePMIDs) == 0 {
		return []string{}, nil
	}
	clause, arguments, err := blockSearchFilter(options.Scope, candidatePMIDs)
	if err != nil {
		return nil, err
	}
	rows, err := e.index.SQL().QueryContext(ctx, `
		SELECT b.pmid, b.searchable_text
		FROM blocks AS b WHERE `+clause+`
		ORDER BY b.pmid, b.id`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("exact PMID block query: %w", err)
	}
	defer rows.Close()
	needed := options.Offset + options.Limit
	matches := make([]string, 0, needed)
	currentPMID := ""
	currentMatched := false
	for rows.Next() && len(matches) < needed {
		var pmid, searchable string
		if err := rows.Scan(&pmid, &searchable); err != nil {
			return nil, err
		}
		if pmid != currentPMID {
			currentPMID = pmid
			currentMatched = false
		}
		if currentMatched || !searchableMatches(searchable, options, regex) {
			continue
		}
		currentMatched = true
		matches = append(matches, pmid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if options.Offset >= len(matches) {
		return []string{}, nil
	}
	return matches[options.Offset:min(needed, len(matches))], nil
}

func blockSearchFilter(scope string, candidatePMIDs []string) (string, []any, error) {
	clause, err := scopeClause(scope)
	if err != nil {
		return "", nil, err
	}
	if candidatePMIDs == nil {
		return clause, nil, nil
	}
	encoded, err := json.Marshal(candidatePMIDs)
	if err != nil {
		return "", nil, err
	}
	return clause + " AND b.pmid IN (SELECT value FROM json_each(?))", []any{string(encoded)}, nil
}

func (e *Engine) markdownPMIDs(
	ctx context.Context,
	options Options,
	regex *regexp.Regexp,
	candidatePMIDs []string,
) ([]string, error) {
	documents, err := e.candidateDocuments(ctx, candidatePMIDs)
	if err != nil {
		return nil, err
	}
	needed := options.Offset + options.Limit
	matches := make([]string, 0, needed)
	workers := min(32, max(1, runtime.GOMAXPROCS(0)))
	batchSize := max(256, workers*8)
	var matcher *lowerMatcher
	if options.Mode == "literal" && !options.CaseSensitive {
		matcher = newLowerMatcher(options.Query)
	}
	for start := 0; start < len(documents) && len(matches) < needed; start += batchSize {
		end := min(start+batchSize, len(documents))
		matched, scanErr := e.scanMarkdownBatch(
			ctx, documents[start:end], options, regex, matcher, workers,
		)
		if scanErr != nil {
			return nil, scanErr
		}
		for position, ok := range matched {
			if ok {
				matches = append(matches, documents[start+position].PMID)
				if len(matches) >= needed {
					break
				}
			}
		}
	}
	if options.Offset >= len(matches) {
		return []string{}, nil
	}
	return matches[options.Offset:min(needed, len(matches))], nil
}

func (e *Engine) scanMarkdownBatch(
	ctx context.Context,
	documents []indexdb.Document,
	options Options,
	regex *regexp.Regexp,
	matcher *lowerMatcher,
	workers int,
) ([]bool, error) {
	matched := make([]bool, len(documents))
	jobs := make(chan int)
	errChannel := make(chan error, 1)
	var group sync.WaitGroup
	for range min(workers, len(documents)) {
		group.Add(1)
		go func() {
			defer group.Done()
			buffer := e.markdownBuffer()
			defer e.releaseMarkdownBuffer(buffer)
			for position := range jobs {
				if ctx.Err() != nil {
					return
				}
				path, err := e.markdownPath(documents[position])
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					select {
					case errChannel <- err:
					default:
					}
					return
				}
				file, err := os.Open(path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					select {
					case errChannel <- err:
					default:
					}
					return
				}
				buffer.Reset()
				_, readErr := buffer.ReadFrom(file)
				closeErr := file.Close()
				if readErr != nil {
					select {
					case errChannel <- readErr:
					default:
					}
					return
				}
				if closeErr != nil {
					select {
					case errChannel <- closeErr:
					default:
					}
					return
				}
				matched[position] = markdownDataMatches(buffer.Bytes(), options, regex, matcher)
			}
		}()
	}
	for position := range documents {
		select {
		case jobs <- position:
		case err := <-errChannel:
			close(jobs)
			group.Wait()
			return nil, err
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	group.Wait()
	select {
	case err := <-errChannel:
		return nil, err
	default:
		return matched, nil
	}
}

func (e *Engine) markdownBuffer() *bytes.Buffer {
	buffer, _ := e.markdownData.Get().(*bytes.Buffer)
	if buffer == nil {
		return &bytes.Buffer{}
	}
	return buffer
}

func (e *Engine) releaseMarkdownBuffer(buffer *bytes.Buffer) {
	if buffer.Cap() > maxPooledMarkdownBytes {
		return
	}
	buffer.Reset()
	e.markdownData.Put(buffer)
}

func (e *Engine) markdownPath(document indexdb.Document) (string, error) {
	if cached, ok := e.markdownPaths.Load(document.PMID); ok {
		return cached.(string), nil
	}
	path, err := e.index.Store().ResolveIndexedSource(
		document.PMID, document.RelativePath, "markdown.md",
	)
	if err != nil {
		return "", err
	}
	e.markdownPaths.Store(document.PMID, path)
	return path, nil
}

func markdownDataMatches(
	data []byte,
	options Options,
	regex *regexp.Regexp,
	matcher *lowerMatcher,
) bool {
	if options.Mode == "regex" {
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if regex.Match(line) {
				return true
			}
		}
		return false
	}
	if strings.ContainsAny(options.Query, "\r\n") {
		return false
	}
	if options.CaseSensitive {
		return bytes.Contains(data, []byte(options.Query))
	}
	return matcher.MatchBytes(data)
}

// lowerMatcher performs allocation-free case-insensitive literal matching with
// the same unicode.ToLower semantics used by the original implementation.
type lowerMatcher struct {
	needle      []rune
	asciiNeedle []byte
}

func newLowerMatcher(query string) *lowerMatcher {
	lowered := strings.ToLower(query)
	matcher := &lowerMatcher{needle: []rune(lowered)}
	if utf8.RuneCountInString(lowered) == len(lowered) {
		matcher.asciiNeedle = []byte(lowered)
	}
	return matcher
}

func (m *lowerMatcher) MatchBytes(data []byte) bool {
	return m.IndexBytes(data) >= 0
}

func (m *lowerMatcher) IndexBytes(data []byte) int {
	if m.asciiNeedle != nil {
		return indexASCIIFold(data, m.asciiNeedle)
	}
	if len(m.needle) == 0 {
		return 0
	}
	for start := 0; start < len(data); {
		first, firstSize := utf8.DecodeRune(data[start:])
		if unicode.ToLower(first) == m.needle[0] {
			position := start + firstSize
			matched := 1
			for matched < len(m.needle) && position < len(data) {
				value, size := utf8.DecodeRune(data[position:])
				if unicode.ToLower(value) != m.needle[matched] {
					break
				}
				position += size
				matched++
			}
			if matched == len(m.needle) {
				return start
			}
		}
		start += firstSize
	}
	return -1
}

func indexASCIIFold(data, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for start := 0; start+len(needle) <= len(data); start++ {
		if foldASCII(data[start]) != needle[0] {
			continue
		}
		matched := true
		for position := 1; position < len(needle); position++ {
			if foldASCII(data[start+position]) != needle[position] {
				matched = false
				break
			}
		}
		if matched {
			return start
		}
	}
	return -1
}

func foldASCII(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

func searchableMatches(searchable string, options Options, regex *regexp.Regexp) bool {
	for _, line := range strings.Split(strings.ReplaceAll(searchable, "\r\n", "\n"), "\n") {
		if lineMatches(line, options, regex) {
			return true
		}
	}
	return false
}

func lineMatches(line string, options Options, regex *regexp.Regexp) bool {
	if options.Mode == "regex" {
		return regex.FindStringIndex(line) != nil
	}
	if !options.CaseSensitive {
		return strings.Contains(strings.ToLower(line), strings.ToLower(options.Query))
	}
	return strings.Contains(line, options.Query)
}

func (e *Engine) searchMarkdown(document indexdb.Document, options Options, regex *regexp.Regexp) ([]Result, error) {
	path, err := e.markdownPath(document)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if options.ContextLines == 0 {
		return e.searchMarkdownNoContext(file, document, options, regex)
	}
	lines := make([]string, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return matchingLines(document, lines, options, regex, "text", "", "paper://"+document.PMID+"/markdown"), nil
}

func (e *Engine) searchMarkdownNoContext(
	file *os.File,
	document indexdb.Document,
	options Options,
	regex *regexp.Regexp,
) ([]Result, error) {
	buffer := e.markdownBuffer()
	defer e.releaseMarkdownBuffer(buffer)
	buffer.Reset()
	if _, err := buffer.ReadFrom(file); err != nil {
		return nil, err
	}
	data := buffer.Bytes()
	results := make([]Result, 0)
	var matcher *lowerMatcher
	if options.Mode == "literal" && !options.CaseSensitive {
		matcher = newLowerMatcher(options.Query)
	}
	for lineNumber := 1; len(data) > 0; lineNumber++ {
		line := data
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			line = data[:newline]
			data = data[newline+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		position := -1
		switch {
		case options.Mode == "regex":
			location := regex.FindIndex(line)
			if location != nil {
				position = location[0]
			}
		case options.CaseSensitive:
			position = bytes.Index(line, []byte(options.Query))
		default:
			position = matcher.IndexBytes(line)
		}
		if position < 0 {
			continue
		}
		lineValue := lineNumber
		bytePosition := position
		results = append(results, Result{
			PMID: document.PMID, Title: document.Title,
			Snippet: truncateRunes(string(line), maxSnippetRunes),
			Line:    &lineValue, Position: &bytePosition, ContentType: "text",
			ResourceURI: "paper://" + document.PMID + "/markdown",
		})
	}
	return results, nil
}

func matchingLines(document indexdb.Document, lines []string, options Options, regex *regexp.Regexp, contentType, blockID, uri string) []Result {
	results := make([]Result, 0)
	lowerQuery := strings.ToLower(options.Query)
	for index, line := range lines {
		position := -1
		if options.Mode == "regex" {
			location := regex.FindStringIndex(line)
			if location != nil {
				position = location[0]
			}
		} else if !options.CaseSensitive {
			position = strings.Index(strings.ToLower(line), lowerQuery)
		} else {
			position = strings.Index(line, options.Query)
		}
		if position < 0 {
			continue
		}
		start := max(0, index-options.ContextLines)
		end := min(len(lines), index+options.ContextLines+1)
		lineNumber := index + 1
		bytePosition := position
		results = append(results, Result{
			PMID: document.PMID, Title: document.Title,
			Snippet: truncateRunes(strings.Join(lines[start:end], "\n"), maxSnippetRunes),
			Line:    &lineNumber, Position: &bytePosition, BlockID: blockID,
			ContentType: contentType, ResourceURI: uri,
		})
	}
	return results
}

func (e *Engine) candidateDocuments(ctx context.Context, pmids []string) ([]indexdb.Document, error) {
	if pmids != nil && len(pmids) == 0 {
		return []indexdb.Document{}, nil
	}
	query := `SELECT pmid, COALESCE(title, ''), relative_path FROM documents`
	arguments := make([]any, 0, 1)
	if pmids != nil {
		encoded, err := json.Marshal(pmids)
		if err != nil {
			return nil, err
		}
		query += ` WHERE pmid IN (SELECT value FROM json_each(?))`
		arguments = append(arguments, string(encoded))
	}
	query += ` ORDER BY pmid`
	rows, err := e.index.SQL().QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	documents := make([]indexdb.Document, 0)
	for rows.Next() {
		var document indexdb.Document
		if err := rows.Scan(&document.PMID, &document.Title, &document.RelativePath); err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	return documents, rows.Err()
}

func normalizeOptions(options *Options) error {
	options.Query = strings.TrimSpace(options.Query)
	if options.Query == "" {
		return errors.New("query is required")
	}
	if options.Mode == "" {
		options.Mode = "ranked"
	}
	if options.Scope == "" {
		options.Scope = "text"
	}
	if options.Output == "" {
		options.Output = "full"
	}
	if options.Output != "pmids" && options.Output != "snippets" && options.Output != "full" {
		return fmt.Errorf("output must be pmids, snippets, or full")
	}
	if options.Limit == 0 {
		options.Limit = config.DefaultQueryLimit
	}
	maxLimit := config.MaxDetailedQueryLimit
	if options.Output == "pmids" {
		maxLimit = config.MaxPMIDQueryLimit
	}
	if options.Limit < 1 || options.Limit > maxLimit {
		return fmt.Errorf("limit must be between 1 and %d for output %q", maxLimit, options.Output)
	}
	if options.Offset < 0 {
		return errors.New("offset cannot be negative")
	}
	if options.ContextLines < 0 || options.ContextLines > config.MaxContextLines {
		return fmt.Errorf("context_lines must be between 0 and %d", config.MaxContextLines)
	}
	if options.Mode != "ranked" && options.Mode != "literal" && options.Mode != "regex" {
		return fmt.Errorf("mode must be ranked, literal, or regex")
	}
	if options.Mode == "ranked" && options.CaseSensitive {
		return errors.New("ranked FTS5 search is case-insensitive; use literal or regex mode for case-sensitive matching")
	}
	if _, err := scopeClause(options.Scope); err != nil && options.Scope != "all" {
		return err
	}
	for _, pmid := range options.PMIDs {
		if err := corpus.ValidatePMID(pmid); err != nil {
			return err
		}
	}
	return nil
}

// safeFTSTerm recognizes the narrow regex form whose FTS candidate filtering
// cannot discard exact matches. Other regexes deliberately fall back to scan.
func safeFTSTerm(pattern string) string {
	matcher := regexp.MustCompile(`^\\b([A-Za-z0-9_]{3,})\\b$`)
	match := matcher.FindStringSubmatch(pattern)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func scopeClause(scope string) (string, error) {
	switch scope {
	case "text":
		return "b.block_type NOT IN ('figure', 'table')", nil
	case "figures":
		return "b.block_type = 'figure'", nil
	case "tables":
		return "b.block_type = 'table'", nil
	case "all":
		return "1 = 1", nil
	default:
		return "", fmt.Errorf("scope must be text, figures, tables, or all")
	}
}

func resourceURI(pmid, contentType, imagePath string) string {
	if contentType == "figure" && imagePath != "" {
		return "paper://" + pmid + "/image/" + filepath.Base(imagePath)
	}
	return "paper://" + pmid + "/structured"
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}
