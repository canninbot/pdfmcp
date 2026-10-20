package index

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/canninbot/pdfmcp/internal/testutil"
)

func TestIncrementalIndexAndDeletedCleanup(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first, err := database.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Indexed != 2 || first.Failed != 0 {
		t.Fatalf("unexpected first sync: %+v", first)
	}
	second, err := database.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Unchanged != 2 || second.Indexed != 0 {
		t.Fatalf("unexpected second sync: %+v", second)
	}
	markdown := filepath.Join(cfg.Root, "12345678", "markdown.md")
	if err := os.WriteFile(markdown, []byte("# changed\nnew content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := database.Sync(ctx, false)
	if err != nil || third.Indexed != 1 {
		t.Fatalf("modified sync: %+v, %v", third, err)
	}
	if err := os.RemoveAll(filepath.Join(cfg.Root, "23456789")); err != nil {
		t.Fatal(err)
	}
	fourth, err := database.Sync(ctx, false)
	if err != nil || fourth.Deleted != 1 {
		t.Fatalf("deleted sync: %+v, %v", fourth, err)
	}
	documents, err := database.ListDocuments(ctx, 10, 0)
	if err != nil || len(documents) != 1 {
		t.Fatalf("documents after cleanup: %v, %v", documents, err)
	}
}

func TestPageDocumentAndStats(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	document, err := database.GetDocument(ctx, "12345678")
	if err != nil || document.Title != "Synthetic 12345678" {
		t.Fatalf("document: %+v, %v", document, err)
	}
	if !bytes.Contains(document.Metadata, []byte(`"mineru"`)) {
		t.Fatalf("middle_json extraction metadata was not preserved: %s", document.Metadata)
	}
	blocks, err := database.PageBlocks(ctx, "12345678", 1)
	if err != nil || len(blocks) != 2 {
		t.Fatalf("page blocks: %v, %v", blocks, err)
	}
	stats, err := database.CorpusStats(ctx)
	if err != nil || stats.Papers != 2 || stats.Figures != 2 || stats.Tables != 2 {
		t.Fatalf("stats: %+v, %v", stats, err)
	}
}

func TestSchemaChangeForcesReindex(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx,
		`UPDATE meta SET value = '1' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	progress, err := database.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Indexed != 2 || progress.Unchanged != 0 {
		t.Fatalf("schema mismatch did not rebuild: %+v", progress)
	}
}

func TestVersionFiveMigratesTrigramsWithoutReindex(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.CreateCorpus(t)
	database, err := Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx,
		`UPDATE meta SET value = '5' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `DELETE FROM fts_documents_trigram`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `DELETE FROM document_search`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if !database.HasTrigramIndex() {
		t.Fatal("trigram index was not detected after migration")
	}
	var indexed int
	if err := database.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM document_search`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 2 {
		t.Fatalf("got %d migrated documents, want 2", indexed)
	}
	progress, err := database.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Unchanged != 2 || progress.Indexed != 0 {
		t.Fatalf("migration unnecessarily reindexed source files: %+v", progress)
	}
}
