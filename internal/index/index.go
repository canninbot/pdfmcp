// Package index manages the rebuildable SQLite FTS5 corpus index.
package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/canninbot/pdfmcp/internal/config"
	"github.com/canninbot/pdfmcp/internal/corpus"
	"github.com/canninbot/pdfmcp/internal/mineru"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS documents (
    pmid TEXT PRIMARY KEY,
    title TEXT,
    relative_path TEXT NOT NULL,
    page_count INTEGER,
    figure_count INTEGER NOT NULL DEFAULT 0,
    table_count INTEGER NOT NULL DEFAULT 0,
    source_fingerprint TEXT NOT NULL,
    indexed_at TEXT NOT NULL,
    status TEXT NOT NULL,
    metadata_json TEXT,
    diagnostics_json TEXT,
    original_pdf TEXT
);
CREATE TABLE IF NOT EXISTS blocks (
    id INTEGER PRIMARY KEY,
    pmid TEXT NOT NULL REFERENCES documents(pmid) ON DELETE CASCADE,
    block_id TEXT NOT NULL,
    page_idx INTEGER,
    block_type TEXT NOT NULL,
    section TEXT,
    text TEXT,
    caption TEXT,
    searchable_text TEXT NOT NULL,
    bbox_json TEXT,
    image_path TEXT,
    source_locator TEXT,
    UNIQUE(pmid, block_id)
);
CREATE INDEX IF NOT EXISTS blocks_pmid_page ON blocks(pmid, page_idx);
CREATE INDEX IF NOT EXISTS blocks_type ON blocks(block_type);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_blocks USING fts5(
    searchable_text,
    section,
    content='blocks',
    content_rowid='id',
    detail=full,
    tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS blocks_ai AFTER INSERT ON blocks BEGIN
    INSERT INTO fts_blocks(rowid, searchable_text, section)
    VALUES (new.id, new.searchable_text, new.section);
END;
CREATE TRIGGER IF NOT EXISTS blocks_ad AFTER DELETE ON blocks BEGIN
    INSERT INTO fts_blocks(fts_blocks, rowid, searchable_text, section)
    VALUES ('delete', old.id, old.searchable_text, old.section);
END;
CREATE TRIGGER IF NOT EXISTS blocks_au AFTER UPDATE ON blocks BEGIN
    INSERT INTO fts_blocks(fts_blocks, rowid, searchable_text, section)
    VALUES ('delete', old.id, old.searchable_text, old.section);
    INSERT INTO fts_blocks(rowid, searchable_text, section)
    VALUES (new.id, new.searchable_text, new.section);
END;
CREATE TABLE IF NOT EXISTS document_search (
    id INTEGER PRIMARY KEY,
    pmid TEXT NOT NULL UNIQUE REFERENCES documents(pmid) ON DELETE CASCADE
);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_documents_trigram USING fts5(
    body,
    figures,
    tables,
    content='',
    contentless_delete=1,
    detail=column,
    tokenize='trigram case_sensitive 1'
);
CREATE TRIGGER IF NOT EXISTS document_search_ad AFTER DELETE ON document_search BEGIN
    DELETE FROM fts_documents_trigram WHERE rowid = old.id;
END;
`

const resetDerivedSchema = `
DROP TRIGGER IF EXISTS document_search_ad;
DROP TRIGGER IF EXISTS blocks_ai;
DROP TRIGGER IF EXISTS blocks_ad;
DROP TRIGGER IF EXISTS blocks_au;
DROP TABLE IF EXISTS fts_documents_trigram;
DROP TABLE IF EXISTS document_search;
DROP TABLE IF EXISTS fts_blocks;
DROP TABLE IF EXISTS blocks;
DROP TABLE IF EXISTS documents;
`

// DB wraps the shared SQLite connection pool.
type DB struct {
	db       *sql.DB
	path     string
	root     string
	store    *corpus.Store
	readOnly bool
	trigram  bool
	// schemaMismatch forces a file-authoritative rebuild on the next sync.
	schemaMismatch bool
}

// Progress summarizes an index synchronization.
type Progress struct {
	Discovered int      `json:"discovered"`
	Indexed    int      `json:"indexed"`
	Unchanged  int      `json:"unchanged"`
	Deleted    int      `json:"deleted"`
	Failed     int      `json:"failed"`
	Messages   []string `json:"messages,omitempty"`
}

// Document describes one indexed paper.
type Document struct {
	PMID         string          `json:"pmid"`
	Title        string          `json:"title,omitempty"`
	RelativePath string          `json:"relative_path"`
	PageCount    *int            `json:"page_count,omitempty"`
	FigureCount  int             `json:"figure_count"`
	TableCount   int             `json:"table_count"`
	IndexedAt    string          `json:"indexed_at"`
	Status       string          `json:"indexing_status"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	Diagnostics  []string        `json:"diagnostics,omitempty"`
	OriginalPDF  string          `json:"original_pdf,omitempty"`
}

// Block is an indexed content record.
type Block struct {
	ID         int64           `json:"-"`
	PMID       string          `json:"pmid"`
	BlockID    string          `json:"block_id"`
	PageIndex  *int            `json:"page_idx,omitempty"`
	Type       string          `json:"content_type"`
	Section    string          `json:"section,omitempty"`
	Text       string          `json:"text,omitempty"`
	Caption    string          `json:"caption,omitempty"`
	BBox       json.RawMessage `json:"bbox,omitempty"`
	ImagePath  string          `json:"image_path,omitempty"`
	Source     string          `json:"source"`
	Searchable string          `json:"-"`
}

// Stats contains corpus-wide index statistics.
type Stats struct {
	Papers             int    `json:"total_indexed_papers"`
	KnownPages         int    `json:"total_pages_with_known_counts"`
	Figures            int    `json:"total_extracted_figures"`
	Tables             int    `json:"total_extracted_tables"`
	Records            int    `json:"total_searchable_records"`
	DatabaseBytes      int64  `json:"sqlite_database_size_bytes"`
	LastIndexedAt      string `json:"last_indexing_time,omitempty"`
	MissingFiles       int    `json:"missing_file_count"`
	MalformedDocuments int    `json:"malformed_file_count"`
}

// Open opens an index and creates its schema when writable.
func Open(ctx context.Context, cfg config.Config, readOnly bool) (*DB, error) {
	store, err := corpus.New(cfg.Root)
	if err != nil {
		return nil, err
	}
	if readOnly {
		if _, err := os.Stat(cfg.Database); err != nil {
			return nil, fmt.Errorf("index unavailable; run pdfmcp index first: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", filepath.ToSlash(cfg.Database))
	if readOnly {
		dsn += "&mode=ro"
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	database.SetMaxOpenConns(8)
	result := &DB{db: database, path: cfg.Database, root: cfg.Root, store: store, readOnly: readOnly}
	if !readOnly {
		if _, err := database.ExecContext(ctx, schema); err != nil {
			database.Close()
			return nil, fmt.Errorf("initialize index: %w", err)
		}
		var storedVersion int
		versionErr := database.QueryRowContext(ctx,
			`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'`).Scan(&storedVersion)
		switch {
		case errors.Is(versionErr, sql.ErrNoRows):
			if _, err := database.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES ('schema_version', ?)`, config.SchemaVersion); err != nil {
				database.Close()
				return nil, fmt.Errorf("write schema version: %w", err)
			}
		case versionErr != nil:
			database.Close()
			return nil, fmt.Errorf("read schema version: %w", versionErr)
		default:
			switch {
			case storedVersion == config.SchemaVersion:
			case (storedVersion == 5 || storedVersion == 6) && config.SchemaVersion == 7:
				if err := result.rebuildDocumentTrigrams(ctx); err != nil {
					database.Close()
					return nil, fmt.Errorf("migrate trigram index: %w", err)
				}
				if _, err := database.ExecContext(ctx,
					`UPDATE meta SET value = ? WHERE key = 'schema_version'`, config.SchemaVersion); err != nil {
					database.Close()
					return nil, fmt.Errorf("write migrated schema version: %w", err)
				}
			default:
				result.schemaMismatch = true
			}
		}
	}
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, fmt.Errorf("ping index: %w", err)
	}
	_ = database.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM sqlite_master
		              WHERE type = 'table' AND name = 'fts_documents_trigram')`).Scan(&result.trigram)
	return result, nil
}

// Close closes the SQLite pool.
func (d *DB) Close() error {
	return d.db.Close()
}

// SQL exposes the database for the search package's parameterized queries.
func (d *DB) SQL() *sql.DB {
	return d.db
}

// Store exposes safe corpus access associated with this index.
func (d *DB) Store() *corpus.Store {
	return d.store
}

// HasTrigramIndex reports whether the optional exact-search accelerator exists.
func (d *DB) HasTrigramIndex() bool {
	return d.trigram
}

// Sync incrementally synchronizes the database with corpus files.
func (d *DB) Sync(ctx context.Context, rebuild bool) (Progress, error) {
	if d.readOnly {
		return Progress{}, errors.New("index was opened read-only")
	}
	rebuild = rebuild || d.schemaMismatch
	if rebuild {
		if _, err := d.db.ExecContext(ctx, resetDerivedSchema); err != nil {
			return Progress{}, fmt.Errorf("reset derived index: %w", err)
		}
		if _, err := d.db.ExecContext(ctx, schema); err != nil {
			return Progress{}, fmt.Errorf("recreate derived index: %w", err)
		}
	}
	pmids, err := d.store.Discover()
	if err != nil {
		return Progress{}, err
	}
	progress := Progress{Discovered: len(pmids)}
	existing := make(map[string]string)
	rows, err := d.db.QueryContext(ctx, `SELECT pmid, source_fingerprint FROM documents`)
	if err != nil {
		return progress, err
	}
	for rows.Next() {
		var pmid, fingerprint string
		if err := rows.Scan(&pmid, &fingerprint); err != nil {
			rows.Close()
			return progress, err
		}
		existing[pmid] = fingerprint
	}
	rows.Close()
	current := make(map[string]struct{}, len(pmids))
	var transaction *sql.Tx
	batchCount := 0
	commitBatch := func() error {
		if transaction == nil {
			return nil
		}
		if err := transaction.Commit(); err != nil {
			return err
		}
		transaction = nil
		batchCount = 0
		return nil
	}
	for _, pmid := range pmids {
		if err := ctx.Err(); err != nil {
			if transaction != nil {
				transaction.Rollback()
			}
			return progress, err
		}
		current[pmid] = struct{}{}
		dir, err := d.store.DocumentDir(pmid)
		if err != nil {
			progress.Failed++
			progress.Messages = append(progress.Messages, fmt.Sprintf("%s: %v", pmid, err))
			continue
		}
		fingerprint, err := sourceFingerprint(dir)
		if err != nil {
			progress.Failed++
			progress.Messages = append(progress.Messages, fmt.Sprintf("%s: %v", pmid, err))
			continue
		}
		if !rebuild && existing[pmid] == fingerprint {
			progress.Unchanged++
			continue
		}
		if transaction == nil {
			transaction, err = d.db.BeginTx(ctx, nil)
			if err != nil {
				return progress, err
			}
		}
		if _, err := transaction.ExecContext(ctx, `SAVEPOINT index_document`); err != nil {
			transaction.Rollback()
			return progress, err
		}
		if err := d.indexDocument(ctx, transaction, pmid, dir, fingerprint); err != nil {
			_, _ = transaction.ExecContext(ctx, `ROLLBACK TO index_document`)
			_, _ = transaction.ExecContext(ctx, `RELEASE index_document`)
			progress.Failed++
			progress.Messages = append(progress.Messages, fmt.Sprintf("%s: %v", pmid, err))
			continue
		}
		if _, err := transaction.ExecContext(ctx, `RELEASE index_document`); err != nil {
			transaction.Rollback()
			return progress, err
		}
		progress.Indexed++
		batchCount++
		if batchCount >= 100 {
			if err := commitBatch(); err != nil {
				return progress, fmt.Errorf("commit index batch: %w", err)
			}
		}
	}
	if err := commitBatch(); err != nil {
		return progress, fmt.Errorf("commit final index batch: %w", err)
	}
	for pmid := range existing {
		if _, ok := current[pmid]; ok {
			continue
		}
		if _, err := d.db.ExecContext(ctx, `DELETE FROM documents WHERE pmid = ?`, pmid); err != nil {
			return progress, fmt.Errorf("delete removed PMID %s: %w", pmid, err)
		}
		progress.Deleted++
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('last_indexed_at', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, now)
	missingFailures := 0
	for _, message := range progress.Messages {
		if strings.Contains(strings.ToLower(message), "no such file") {
			missingFailures++
		}
	}
	for key, value := range map[string]int{
		"last_missing_failures":   missingFailures,
		"last_malformed_failures": progress.Failed - missingFailures,
	} {
		if _, metaErr := d.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err == nil {
			err = metaErr
		}
	}
	if err == nil {
		_, err = d.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES ('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, config.SchemaVersion)
		if err == nil {
			d.schemaMismatch = false
		}
	}
	if err == nil {
		// Indexing uses WAL for concurrent readers; truncate the temporary WAL
		// after a completed maintenance run so the durable artifact is one DB.
		_, err = d.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	return progress, err
}

func (d *DB) indexDocument(ctx context.Context, tx *sql.Tx, pmid, dir, fingerprint string) error {
	doc, parseErr := mineru.ParseDirectory(dir)
	status := "indexed"
	diagnostics := append([]string(nil), doc.Diagnostics...)
	if parseErr != nil {
		diagnostics = append(diagnostics, parseErr.Error())
		markdown, err := os.ReadFile(filepath.Join(dir, "markdown.md"))
		if err != nil {
			return fmt.Errorf("no usable structured content or Markdown: %w", parseErr)
		}
		doc.Blocks = markdownBlocks(string(markdown))
		status = "indexed_markdown_fallback"
	}
	figures := 0
	tables := 0
	for _, block := range doc.Blocks {
		switch block.Type {
		case "figure":
			figures++
		case "table":
			tables++
		}
	}
	relative, err := filepath.Rel(d.root, dir)
	if err != nil {
		return err
	}
	originalPDF := ""
	if path, err := d.store.PDF(pmid); err == nil {
		originalPDF = filepath.Base(path)
	}
	diagnosticsJSON, _ := json.Marshal(diagnostics)
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE pmid = ?`, pmid); err != nil {
		return err
	}
	var pageCount any
	if doc.PageCount != nil {
		pageCount = *doc.PageCount
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO documents(
		    pmid, title, relative_path, page_count, figure_count, table_count,
		    source_fingerprint, indexed_at, status, metadata_json,
		    diagnostics_json, original_pdf
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pmid, nullable(doc.Title), filepath.ToSlash(relative), pageCount, figures, tables,
		fingerprint, time.Now().UTC().Format(time.RFC3339), status, nullable(string(doc.Metadata)),
		string(diagnosticsJSON), nullable(originalPDF)); err != nil {
		return err
	}
	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO blocks(
		    pmid, block_id, page_idx, block_type, section, text, caption,
		    searchable_text, bbox_json, image_path, source_locator
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	seen := make(map[string]int)
	var bodyText, figureText, tableText strings.Builder
	for _, block := range doc.Blocks {
		blockID := block.ID
		if duplicate := seen[blockID]; duplicate > 0 {
			blockID = fmt.Sprintf("%s-%d", blockID, duplicate)
		}
		seen[block.ID]++
		searchable := block.Text
		if block.Caption != "" && !strings.Contains(searchable, block.Caption) {
			searchable = strings.TrimSpace(searchable + "\n" + block.Caption)
		}
		bboxJSON := ""
		if block.BBox != nil {
			encoded, _ := json.Marshal(block.BBox)
			bboxJSON = string(encoded)
		}
		var pageIndex any
		if block.PageIndex != nil {
			pageIndex = *block.PageIndex
		}
		sourceLocator := fmt.Sprintf("%s#block=%d", block.Source, block.SourceIndex)
		if _, err := statement.ExecContext(ctx, pmid, blockID, pageIndex,
			block.Type, nullable(block.Section), nullable(block.Text), nullable(block.Caption),
			searchable, nullable(bboxJSON), nullable(block.ImagePath), sourceLocator); err != nil {
			return fmt.Errorf("insert block %s: %w", blockID, err)
		}
		switch block.Type {
		case "figure":
			appendSearchText(&figureText, searchable)
		case "table":
			appendSearchText(&tableText, searchable)
		default:
			appendSearchText(&bodyText, searchable)
		}
	}
	return insertDocumentTrigrams(ctx, tx, pmid, bodyText.String(), figureText.String(), tableText.String())
}

func (d *DB) rebuildDocumentTrigrams(ctx context.Context) error {
	transaction, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `DELETE FROM fts_documents_trigram`); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM document_search`); err != nil {
		return err
	}
	rows, err := transaction.QueryContext(ctx, `
		SELECT pmid,
		       COALESCE(group_concat(CASE WHEN block_type NOT IN ('figure', 'table')
		                                  THEN searchable_text END, char(10)), ''),
		       COALESCE(group_concat(CASE WHEN block_type = 'figure'
		                                  THEN searchable_text END, char(10)), ''),
		       COALESCE(group_concat(CASE WHEN block_type = 'table'
		                                  THEN searchable_text END, char(10)), '')
		FROM blocks GROUP BY pmid ORDER BY pmid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pmid, body, figures, tables string
		if err := rows.Scan(&pmid, &body, &figures, &tables); err != nil {
			return err
		}
		if err := insertDocumentTrigrams(ctx, transaction, pmid, body, figures, tables); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return transaction.Commit()
}

func insertDocumentTrigrams(
	ctx context.Context,
	transaction *sql.Tx,
	pmid string,
	body string,
	figures string,
	tables string,
) error {
	result, err := transaction.ExecContext(ctx, `INSERT INTO document_search(pmid) VALUES (?)`, pmid)
	if err != nil {
		return err
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO fts_documents_trigram(rowid, body, figures, tables)
		VALUES (?, ?, ?, ?)`, rowID, foldForTrigrams(body), foldForTrigrams(figures), foldForTrigrams(tables))
	return err
}

// foldForTrigrams maps every Unicode simple-fold class to one stable rune.
// It is a candidate-index normalization only; exact search still verifies the
// original text using the requested literal or RE2 semantics.
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

func appendSearchText(builder *strings.Builder, text string) {
	if text == "" {
		return
	}
	if builder.Len() > 0 {
		builder.WriteByte('\n')
	}
	builder.WriteString(text)
}

// ListDocuments returns stable PMID-ordered documents with pagination.
func (d *DB) ListDocuments(ctx context.Context, limit, offset int) ([]Document, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT pmid, COALESCE(title, ''), relative_path, page_count,
		       figure_count, table_count, indexed_at, status,
		       COALESCE(metadata_json, ''), COALESCE(diagnostics_json, '[]'),
		       COALESCE(original_pdf, '')
		FROM documents ORDER BY pmid LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocuments(rows)
}

// GetDocument returns one indexed document.
func (d *DB) GetDocument(ctx context.Context, pmid string) (Document, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT pmid, COALESCE(title, ''), relative_path, page_count,
		       figure_count, table_count, indexed_at, status,
		       COALESCE(metadata_json, ''), COALESCE(diagnostics_json, '[]'),
		       COALESCE(original_pdf, '')
		FROM documents WHERE pmid = ?`, pmid)
	if err != nil {
		return Document{}, err
	}
	defer rows.Close()
	documents, err := scanDocuments(rows)
	if err != nil {
		return Document{}, err
	}
	if len(documents) == 0 {
		return Document{}, fs.ErrNotExist
	}
	return documents[0], nil
}

// PageBlocks returns MinerU blocks in reading order for a zero-based page.
func (d *DB) PageBlocks(ctx context.Context, pmid string, pageIndex int) ([]Block, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, pmid, block_id, page_idx, block_type, COALESCE(section, ''),
		       COALESCE(text, ''), COALESCE(caption, ''), COALESCE(bbox_json, ''),
		       COALESCE(image_path, ''), COALESCE(source_locator, ''), searchable_text
		FROM blocks WHERE pmid = ? AND page_idx = ? ORDER BY id`, pmid, pageIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanBlocks(rows)
}

// AllBlocks returns content records for metadata inventories.
func (d *DB) AllBlocks(ctx context.Context, pmid string) ([]Block, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, pmid, block_id, page_idx, block_type, COALESCE(section, ''),
		       COALESCE(text, ''), COALESCE(caption, ''), COALESCE(bbox_json, ''),
		       COALESCE(image_path, ''), COALESCE(source_locator, ''), searchable_text
		FROM blocks WHERE pmid = ? ORDER BY id`, pmid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanBlocks(rows)
}

// FindBlock returns a block by its stable document-local identifier.
func (d *DB) FindBlock(ctx context.Context, pmid, blockID string) (Block, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, pmid, block_id, page_idx, block_type, COALESCE(section, ''),
		       COALESCE(text, ''), COALESCE(caption, ''), COALESCE(bbox_json, ''),
		       COALESCE(image_path, ''), COALESCE(source_locator, ''), searchable_text
		FROM blocks WHERE pmid = ? AND block_id = ?`, pmid, blockID)
	if err != nil {
		return Block{}, err
	}
	defer rows.Close()
	blocks, err := ScanBlocks(rows)
	if err != nil {
		return Block{}, err
	}
	if len(blocks) == 0 {
		return Block{}, fs.ErrNotExist
	}
	return blocks[0], nil
}

// CorpusStats computes aggregate statistics without using search.
func (d *DB) CorpusStats(ctx context.Context) (Stats, error) {
	var stats Stats
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(page_count), 0), COALESCE(SUM(figure_count), 0),
		       COALESCE(SUM(table_count), 0),
		       COALESCE(SUM(CASE WHEN status != 'indexed' AND diagnostics_json NOT LIKE '%no such file%' THEN 1 ELSE 0 END), 0)
		FROM documents`).Scan(&stats.Papers, &stats.KnownPages, &stats.Figures,
		&stats.Tables, &stats.MalformedDocuments)
	if err != nil {
		return stats, err
	}
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks`).Scan(&stats.Records); err != nil {
		return stats, err
	}
	if err := d.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN diagnostics_json LIKE '%no such file%' THEN 1 ELSE 0 END), 0)
		FROM documents`).Scan(&stats.MissingFiles); err != nil {
		return stats, err
	}
	var failedMissing, failedMalformed string
	_ = d.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'last_missing_failures'`).Scan(&failedMissing)
	_ = d.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'last_malformed_failures'`).Scan(&failedMalformed)
	if value, err := strconv.Atoi(failedMissing); err == nil {
		stats.MissingFiles += value
	}
	if value, err := strconv.Atoi(failedMalformed); err == nil {
		stats.MalformedDocuments += value
	}
	_ = d.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'last_indexed_at'`).Scan(&stats.LastIndexedAt)
	if info, err := os.Stat(d.path); err == nil {
		stats.DatabaseBytes = info.Size()
	}
	return stats, nil
}

// ScanBlocks scans the canonical block query column order.
func ScanBlocks(rows *sql.Rows) ([]Block, error) {
	blocks := make([]Block, 0)
	for rows.Next() {
		var block Block
		var page sql.NullInt64
		var bbox string
		if err := rows.Scan(&block.ID, &block.PMID, &block.BlockID, &page, &block.Type,
			&block.Section, &block.Text, &block.Caption, &bbox, &block.ImagePath,
			&block.Source, &block.Searchable); err != nil {
			return nil, err
		}
		if page.Valid {
			value := int(page.Int64)
			block.PageIndex = &value
		}
		if bbox != "" {
			block.BBox = json.RawMessage(bbox)
		}
		blocks = append(blocks, block)
	}
	return blocks, rows.Err()
}

func scanDocuments(rows *sql.Rows) ([]Document, error) {
	documents := make([]Document, 0)
	for rows.Next() {
		var document Document
		var pageCount sql.NullInt64
		var metadata, diagnostics string
		if err := rows.Scan(&document.PMID, &document.Title, &document.RelativePath,
			&pageCount, &document.FigureCount, &document.TableCount, &document.IndexedAt,
			&document.Status, &metadata, &diagnostics, &document.OriginalPDF); err != nil {
			return nil, err
		}
		if pageCount.Valid {
			value := int(pageCount.Int64)
			document.PageCount = &value
		}
		if metadata != "" {
			document.Metadata = json.RawMessage(metadata)
		}
		_ = json.Unmarshal([]byte(diagnostics), &document.Diagnostics)
		documents = append(documents, document)
	}
	return documents, rows.Err()
}

func sourceFingerprint(dir string) (string, error) {
	type entry struct {
		path    string
		size    int64
		modTime int64
	}
	entries := make([]entry, 0)
	err := filepath.WalkDir(dir, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.Type()&os.ModeSymlink != 0 {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{path: filepath.ToSlash(relative), size: info.Size(), modTime: info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	hash := sha256.New()
	for _, item := range entries {
		fmt.Fprintf(hash, "%s\x00%d\x00%d\n", item.path, item.size, item.modTime)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func markdownBlocks(markdown string) []mineru.Block {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	blocks := make([]mineru.Block, 0, (len(lines)+39)/40)
	section := ""
	for start := 0; start < len(lines); start += 40 {
		end := min(start+40, len(lines))
		for _, line := range lines[start:end] {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				section = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
			}
		}
		text := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		if text == "" {
			continue
		}
		blocks = append(blocks, mineru.Block{
			ID: fmt.Sprintf("markdown-lines-%d-%d", start+1, end), Type: "text",
			Section: section, Text: text, Source: "markdown.md", SourceIndex: start,
		})
	}
	return blocks
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
