// Package testutil creates a deterministic synthetic MinerU corpus for tests.
package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/canninbot/pdfmcp/internal/config"
)

// CreateCorpus writes two representative papers and returns configuration.
func CreateCorpus(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	writePaper(t, root, "12345678", true)
	writePaper(t, root, "23456789", false)
	cfg, err := config.New(root, "")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// WriteMinimalPDF writes a tiny valid one-page PDF to path.
func WriteMinimalPDF(t *testing.T, path string) {
	t.Helper()
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		"<< /Length 43 >>\nstream\nBT /F1 18 Tf 25 100 Td (PDFMCP test) Tj ET\nendstream",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for index := 1; index <= len(objects); index++ {
		fmt.Fprintf(&output, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	if err := os.WriteFile(path, output.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePaper(t *testing.T, root, pmid string, includePDF bool) {
	t.Helper()
	dir := filepath.Join(root, pmid)
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := "control content ſin3-only"
	if includePDF {
		marker = "SIN3A NCT04350463 KRAS IL-6 p53 β-catenin thrombocytopenia"
	}
	markdown := "# Synthetic paper " + pmid + "\n\nTEAD inhibition reduces tumor growth.\n\n" + marker + "\n\nA second context line.\n"
	if err := os.WriteFile(filepath.Join(dir, "markdown.md"), []byte(markdown), 0o644); err != nil {
		t.Fatal(err)
	}
	structured := map[string]any{
		"metadata": map[string]any{"document": map[string]any{"title": "Synthetic " + pmid, "page_count": 2}},
		"pages": []any{
			map[string]any{"page_idx": 0, "blocks": []any{
				map[string]any{"type": "doc_title", "index": 0, "bbox": []any{0.1, 0.1, 0.9, 0.2}, "content": "Synthetic " + pmid},
				map[string]any{"type": "text", "index": 1, "bbox": []any{0.1, 0.2, 0.9, 0.4}, "content": "TEAD inhibition reduces tumor growth. " + marker},
			}},
			map[string]any{"page_idx": 1, "blocks": []any{
				map[string]any{"type": "image", "index": 0, "bbox": []any{0.2, 0.2, 0.8, 0.7}, "captions": []any{map[string]any{"content": "Figure showing TEAD pathway"}}, "image_source": "images/page_1_image_0.png"},
				map[string]any{"type": "table", "index": 1, "bbox": []any{0.1, 0.2, 0.9, 0.8}, "content": "Treatment | response\nTEAD inhibitor | high", "caption": "Response table"},
			}},
		},
	}
	writeJSON(t, filepath.Join(dir, "structured_content.json"), structured)
	writeJSON(t, filepath.Join(dir, "middle_json.json"), map[string]any{
		"metadata": map[string]any{"producer": map[string]any{"name": "mineru", "version": "test"}},
		"pages":    structured["pages"],
	})
	imageData := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := range 12 {
		for x := range 16 {
			imageData.Set(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 15), B: 100, A: 255})
		}
	}
	imageFile, err := os.Create(filepath.Join(dir, "images", "page_1_image_0.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(imageFile, imageData); err != nil {
		t.Fatal(err)
	}
	if err := imageFile.Close(); err != nil {
		t.Fatal(err)
	}
	if includePDF {
		WriteMinimalPDF(t, filepath.Join(dir, pmid+".pdf"))
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
