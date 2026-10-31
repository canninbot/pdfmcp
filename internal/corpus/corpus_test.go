package corpus

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/canninbot/pdfmcp/internal/testutil"
)

func TestDiscoveryAndSafePaths(t *testing.T) {
	cfg := testutil.CreateCorpus(t)
	store, err := New(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	pmids, err := store.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(pmids) != 2 || pmids[0] != "12345678" {
		t.Fatalf("unexpected discovery: %v", pmids)
	}
	if _, err := store.ResolveSource("12345678", "../secret"); err == nil {
		t.Fatal("path traversal succeeded")
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.Root, "12345678", "images", "escape.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveImage("12345678", "escape.png"); err == nil {
		t.Fatal("symlink escape succeeded")
	}
}

func TestDiscoverMarkdownCachesResolvedDocuments(t *testing.T) {
	cfg := testutil.CreateCorpus(t)
	store, err := New(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := store.DiscoverMarkdown()
	if err != nil {
		t.Fatal(err)
	}
	pmids := make([]string, 0, len(documents))
	for _, document := range documents {
		pmids = append(pmids, document.PMID)
		if filepath.Base(document.Path) != "markdown.md" {
			t.Fatalf("unexpected Markdown path: %s", document.Path)
		}
	}
	if !slices.Equal(pmids, []string{"12345678", "23456789"}) {
		t.Fatalf("unexpected Markdown documents: %v", pmids)
	}

	missingPath := filepath.Join(cfg.Root, "23456789", "markdown.md")
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}
	documents, err = store.DiscoverMarkdown()
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].PMID != "12345678" {
		t.Fatalf("document without Markdown was included: %+v", documents)
	}
	if _, err := store.DocumentDir("23456789"); !os.IsNotExist(err) {
		t.Fatalf("document cache retained missing Markdown: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, missingPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DiscoverMarkdown(); err == nil {
		t.Fatal("Markdown symlink escape succeeded")
	}
}

func TestDiscoverMarkdownIgnoresUnusableMarkdownOnlyEntries(t *testing.T) {
	cfg := testutil.CreateCorpus(t)
	brokenDirectory := filepath.Join(cfg.Root, "34567890")
	if err := os.Mkdir(brokenDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(cfg.Root, "missing.md"),
		filepath.Join(brokenDirectory, "markdown.md"),
	); err != nil {
		t.Fatal(err)
	}
	nonregularDirectory := filepath.Join(cfg.Root, "45678901")
	if err := os.MkdirAll(filepath.Join(nonregularDirectory, "markdown.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := New(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := store.DiscoverMarkdown()
	if err != nil {
		t.Fatal(err)
	}
	pmids := make([]string, 0, len(documents))
	for _, document := range documents {
		pmids = append(pmids, document.PMID)
	}
	if !slices.Equal(pmids, []string{"12345678", "23456789"}) {
		t.Fatalf("unexpected Markdown documents: %v", pmids)
	}
	structuredPath := filepath.Join(nonregularDirectory, "structured_content.json")
	if err := os.WriteFile(structuredPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DiscoverMarkdown(); err == nil {
		t.Fatal("non-regular Markdown in a MinerU document was accepted")
	}
}

func TestResolveIndexedSourceWithoutDiscovery(t *testing.T) {
	cfg := testutil.CreateCorpus(t)
	store, err := New(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.ResolveIndexedSource("12345678", "12345678", "markdown.md")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "markdown.md" {
		t.Fatalf("unexpected indexed source: %s", path)
	}
	if _, err := store.ResolveIndexedSource("12345678", "..", "markdown.md"); err == nil {
		t.Fatal("indexed directory traversal succeeded")
	}
}
