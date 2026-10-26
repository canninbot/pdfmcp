package index

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/canninbot/pdfmcp/internal/testutil"
)

func TestImportTypedAnnotationsSeparately(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "annotations.csv")
	testutil.WriteAnnotationsCSV(t, path)
	progress, err := database.ImportAnnotations(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Rows != 3 || progress.Imported != 2 || progress.Unmatched != 1 || progress.Source != "annotations.csv" {
		t.Fatalf("unexpected annotation progress: %+v", progress)
	}
	warningFields := make(map[string]struct{}, len(progress.Warnings))
	for _, warning := range progress.Warnings {
		warningFields[warning.Field] = struct{}{}
	}
	if _, ok := warningFields["study_designs"]; !ok {
		t.Fatalf("sparse study_designs warning missing: %+v", progress.Warnings)
	}
	if _, ok := warningFields["pmcid"]; ok {
		t.Fatalf("pmcid warning was not exempted: %+v", progress.Warnings)
	}

	annotation, err := database.GetAnnotation(ctx, "12345678")
	if err != nil {
		t.Fatal(err)
	}
	if len(annotation.CompletenessWarnings) == 0 {
		t.Fatal("annotation resource omitted completeness warnings")
	}
	var values map[string]any
	if err := json.Unmarshal(annotation.Data, &values); err != nil {
		t.Fatal(err)
	}
	if values["pmc_fulltext_available"] != true || values["publication_year"] != float64(2024) ||
		values["scimago_sjr"] != 2.047 {
		t.Fatalf("typed scalar values were not preserved: %+v", values)
	}
	designs, ok := values["study_designs"].([]any)
	if !ok || len(designs) != 2 || designs[0] != "Clinical Trial" {
		t.Fatalf("typed list was not preserved: %+v", values["study_designs"])
	}
	location, ok := values["best_oa_location"].(map[string]any)
	if !ok || location["license"] != "cc-by" {
		t.Fatalf("typed JSON object was not preserved: %+v", values["best_oa_location"])
	}

	var contentBlocks, annotationRows int
	if err := database.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks`).Scan(&contentBlocks); err != nil {
		t.Fatal(err)
	}
	if err := database.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM fts_annotations`).Scan(&annotationRows); err != nil {
		t.Fatal(err)
	}
	if contentBlocks != 8 || annotationRows != 2 {
		t.Fatalf("annotations were not isolated: blocks=%d annotations=%d", contentBlocks, annotationRows)
	}
	stats, err := database.CorpusStats(ctx)
	if err != nil || stats.Records != 8 || stats.AnnotatedPapers != 2 {
		t.Fatalf("unexpected stats: %+v, %v", stats, err)
	}

	markdown := filepath.Join(cfg.Root, "12345678", "markdown.md")
	if err := os.WriteFile(markdown, []byte("# changed\nmetadata remains separate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetAnnotation(ctx, "12345678"); err != nil {
		t.Fatalf("document reindex removed its separate annotation: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(cfg.Root, "23456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	warnings, err := database.AnnotationCompletenessWarnings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range warnings {
		if warning.Field == "study_designs" {
			t.Fatalf("completeness was not refreshed after document removal: %+v", warnings)
		}
	}
}
