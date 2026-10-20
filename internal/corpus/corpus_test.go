package corpus

import (
	"os"
	"path/filepath"
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
