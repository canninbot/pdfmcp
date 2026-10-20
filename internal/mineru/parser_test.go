package mineru

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAlternateSchemaAndBBox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alternate.json")
	data := []byte(`{
        "data": {"page_list": [{"page_index": 3, "items": [
            {"category": "section", "uid": "heading", "text": "Methods"},
            {"category": "paragraph", "uid": "body", "content": [{"type": "text", "content": "alternate text"}], "bbox": [0.1, 0.2, 0.8, 0.9]}
        ]}]}}
    `)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	document, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Blocks) != 2 || document.Blocks[1].Section != "Methods" {
		t.Fatalf("unexpected blocks: %#v", document.Blocks)
	}
	if document.Blocks[1].BBox == nil || document.Blocks[1].BBox.Normalized == nil {
		t.Fatal("normalized bbox was not retained")
	}
	unknown := NormalizeBBox([]float64{10, 20, 300, 400}, "", 612, 792)
	if unknown.Normalized != nil || unknown.Convention != "unknown" {
		t.Fatalf("unknown coordinate system was guessed: %#v", unknown)
	}
	points := NormalizeBBox([]float64{61.2, 79.2, 306, 396}, "pdf_points", 612, 792)
	if points.Normalized == nil {
		t.Fatal("explicit PDF-point bbox was not normalized")
	}
}

func TestMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"pages":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(path); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestDocumentTitleBlockWithoutMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "title.json")
	data := []byte(`{
		"pages":[{"page_idx":0,"blocks":[{"type":"doc_title","content":"Available title"}]}],
		"metadata":{"document":{"title":"doi:technical-identifier"}}
	}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	document, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if document.Title != "Available title" || document.Blocks[0].Type != "doc_title" {
		t.Fatalf("document title was not preserved: %+v", document)
	}
}
