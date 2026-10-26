package index

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/canninbot/pdfmcp/internal/corpus"
)

const annotationCompletenessThreshold = 0.75

var annotationWarningExceptions = map[string]struct{}{
	"pmcid":  {},
	"pmcids": {},
}

var annotationBooleanFields = map[string]struct{}{
	"pmc_fulltext_available": {},
	"open_access.is_oa":      {},
}

var annotationIntegerFields = map[string]struct{}{
	"publication_year":    {},
	"cited_by_count":      {},
	"scimago_match_count": {},
	"scimago_rank":        {},
	"scimago_h_index":     {},
}

var annotationFloatFields = map[string]struct{}{
	"scimago_sjr": {},
}

var annotationJSONFields = map[string]struct{}{
	"best_oa_location": {},
}

var annotationListDelimiters = map[string]string{
	"study_designs":         "|",
	"issns":                 "|",
	"scimago_matched_issns": "|",
	"scimago_issns":         ",",
	"scimago_categories":    ";",
	"scimago_areas":         ";",
}

// AnnotationImportProgress summarizes one replacement annotation import.
type AnnotationImportProgress struct {
	Source    string                          `json:"source"`
	Rows      int                             `json:"rows"`
	Imported  int                             `json:"imported"`
	Unmatched int                             `json:"unmatched"`
	Warnings  []AnnotationCompletenessWarning `json:"completeness_warnings,omitempty"`
}

// AnnotationCompletenessWarning describes one sparse metadata field.
type AnnotationCompletenessWarning struct {
	Field        string  `json:"field"`
	PresentCount int     `json:"present_count"`
	TotalCount   int     `json:"total_count"`
	Completeness float64 `json:"completeness"`
	Message      string  `json:"message"`
}

// Annotation is one typed external metadata record and its import provenance.
type Annotation struct {
	Data                 json.RawMessage                 `json:"data"`
	Source               string                          `json:"source"`
	ImportedAt           string                          `json:"imported_at"`
	CompletenessWarnings []AnnotationCompletenessWarning `json:"completeness_warnings,omitempty"`
}

type annotationRow struct {
	PMID       string
	JSON       string
	Searchable string
	Present    []string
}

type annotationCSV struct {
	Columns []string
	Rows    []annotationRow
}

// ImportAnnotations replaces indexed metadata annotations from a CSV file.
//
// The expected format is the PMID-keyed output produced by
// scripts/annotate_pmids_citations_impact.py. Unknown columns are retained as
// strings so later script versions remain forward compatible.
//
// Args:
//   - ctx: Cancellation and database operation context.
//   - path: CSV file containing one annotation row per PMID.
//
// Returns:
//   - Import counts, including rows without a corresponding corpus document.
func (d *DB) ImportAnnotations(ctx context.Context, path string) (AnnotationImportProgress, error) {
	if d.readOnly {
		return AnnotationImportProgress{}, errors.New("index was opened read-only")
	}
	if !d.annotations {
		return AnnotationImportProgress{}, errors.New("annotation schema is unavailable")
	}
	input, err := readAnnotationCSV(path)
	if err != nil {
		return AnnotationImportProgress{}, err
	}
	progress := AnnotationImportProgress{Source: filepath.Base(path), Rows: len(input.Rows)}
	transaction, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return progress, err
	}
	defer transaction.Rollback()

	if _, err := transaction.ExecContext(ctx, `DELETE FROM fts_annotations`); err != nil {
		return progress, fmt.Errorf("clear annotation search index: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM document_annotations`); err != nil {
		return progress, fmt.Errorf("clear annotations: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM annotation_field_stats`); err != nil {
		return progress, fmt.Errorf("clear annotation completeness: %w", err)
	}
	annotationStatement, err := transaction.PrepareContext(ctx, `
		INSERT INTO document_annotations(
		    pmid, annotation_json, searchable_text, source_name, imported_at
		) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return progress, err
	}
	defer annotationStatement.Close()
	searchStatement, err := transaction.PrepareContext(ctx, `
		INSERT INTO fts_annotations(pmid, searchable_text) VALUES (?, ?)`)
	if err != nil {
		return progress, err
	}
	defer searchStatement.Close()

	importedAt := time.Now().UTC().Format(time.RFC3339)
	presentCounts := make(map[string]int, len(input.Columns))
	for _, row := range input.Rows {
		var exists bool
		if err := transaction.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM documents WHERE pmid = ?)`, row.PMID).Scan(&exists); err != nil {
			return progress, err
		}
		if !exists {
			progress.Unmatched++
			continue
		}
		if _, err := annotationStatement.ExecContext(ctx, row.PMID, row.JSON,
			row.Searchable, progress.Source, importedAt); err != nil {
			return progress, fmt.Errorf("import annotation for PMID %s: %w", row.PMID, err)
		}
		if _, err := searchStatement.ExecContext(ctx, row.PMID, row.Searchable); err != nil {
			return progress, fmt.Errorf("index annotation for PMID %s: %w", row.PMID, err)
		}
		for _, field := range row.Present {
			presentCounts[field]++
		}
		progress.Imported++
	}
	statsStatement, err := transaction.PrepareContext(ctx, `
		INSERT INTO annotation_field_stats(
		    field_name, present_count, total_count, completeness
		) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return progress, err
	}
	defer statsStatement.Close()
	for _, field := range input.Columns {
		completeness := 0.0
		if progress.Imported > 0 {
			completeness = float64(presentCounts[field]) / float64(progress.Imported)
		}
		if _, err := statsStatement.ExecContext(ctx, field, presentCounts[field],
			progress.Imported, completeness); err != nil {
			return progress, fmt.Errorf("store completeness for %s: %w", field, err)
		}
	}
	progress.Warnings = completenessWarnings(input.Columns, presentCounts, progress.Imported)
	if err := transaction.Commit(); err != nil {
		return progress, err
	}
	return progress, nil
}

// AnnotationCompletenessWarnings returns sparse-field warnings for metadata use.
func (d *DB) AnnotationCompletenessWarnings(ctx context.Context) ([]AnnotationCompletenessWarning, error) {
	if !d.annotationStats {
		return []AnnotationCompletenessWarning{}, nil
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT field_name, present_count, total_count, completeness
		FROM annotation_field_stats
		WHERE total_count > 0 AND completeness < ?
		ORDER BY completeness, field_name`, annotationCompletenessThreshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	warnings := make([]AnnotationCompletenessWarning, 0)
	for rows.Next() {
		var warning AnnotationCompletenessWarning
		if err := rows.Scan(&warning.Field, &warning.PresentCount,
			&warning.TotalCount, &warning.Completeness); err != nil {
			return nil, err
		}
		if isAnnotationWarningException(warning.Field) {
			continue
		}
		warning.Message = annotationCompletenessMessage(warning)
		warnings = append(warnings, warning)
	}
	return warnings, rows.Err()
}

func (d *DB) rebuildAnnotationFieldStats(ctx context.Context) error {
	if !d.annotationStats {
		return nil
	}
	if _, err := d.db.ExecContext(ctx, `
		UPDATE annotation_field_stats
		SET total_count = (SELECT COUNT(*) FROM document_annotations),
		    present_count = (
		        SELECT COUNT(*)
		        FROM document_annotations AS a, json_each(a.annotation_json) AS value
		        WHERE value.key = annotation_field_stats.field_name
		          AND value.type != 'null'
		    )`); err != nil {
		return err
	}
	_, err := d.db.ExecContext(ctx, `
		UPDATE annotation_field_stats
		SET completeness = CASE
		    WHEN total_count = 0 THEN 0.0
		    ELSE CAST(present_count AS REAL) / total_count
		END`)
	return err
}

// GetAnnotation returns the typed external metadata annotation for one PMID.
func (d *DB) GetAnnotation(ctx context.Context, pmid string) (Annotation, error) {
	if !d.annotations {
		return Annotation{}, sql.ErrNoRows
	}
	var annotation Annotation
	var raw string
	err := d.db.QueryRowContext(ctx, `
		SELECT annotation_json, source_name, imported_at
		FROM document_annotations
		WHERE pmid = ? AND EXISTS(SELECT 1 FROM documents WHERE pmid = ?)`, pmid, pmid).
		Scan(&raw, &annotation.Source, &annotation.ImportedAt)
	if err != nil {
		return Annotation{}, err
	}
	annotation.Data = json.RawMessage(raw)
	annotation.CompletenessWarnings, err = d.AnnotationCompletenessWarnings(ctx)
	if err != nil {
		return Annotation{}, err
	}
	return annotation, nil
}

func readAnnotationCSV(path string) (annotationCSV, error) {
	file, err := os.Open(path)
	if err != nil {
		return annotationCSV{}, fmt.Errorf("open annotations CSV: %w", err)
	}
	defer file.Close()
	reader := csv.NewReader(file)
	header, err := reader.Read()
	if err != nil {
		return annotationCSV{}, fmt.Errorf("read annotations CSV header: %w", err)
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	pmidColumn, err := validateAnnotationHeader(header)
	if err != nil {
		return annotationCSV{}, err
	}
	seen := make(map[string]struct{})
	rows := make([]annotationRow, 0)
	for line := 2; ; line++ {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return annotationCSV{}, fmt.Errorf("read annotations CSV line %d: %w", line, readErr)
		}
		pmid := strings.TrimSpace(record[pmidColumn])
		if err := corpus.ValidatePMID(pmid); err != nil {
			return annotationCSV{}, fmt.Errorf("annotations CSV line %d: %w", line, err)
		}
		if _, duplicate := seen[pmid]; duplicate {
			return annotationCSV{}, fmt.Errorf("annotations CSV line %d: duplicate PMID %s", line, pmid)
		}
		seen[pmid] = struct{}{}
		values := make(map[string]any, len(header))
		var searchable strings.Builder
		present := make([]string, 0, len(header))
		for position, name := range header {
			value, convertErr := convertAnnotationValue(name, record[position])
			if convertErr != nil {
				return annotationCSV{}, fmt.Errorf("annotations CSV line %d column %q: %w", line, name, convertErr)
			}
			values[name] = value
			if value != nil {
				present = append(present, name)
			}
			appendAnnotationSearchValue(&searchable, name, value)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return annotationCSV{}, fmt.Errorf("encode annotation for PMID %s: %w", pmid, err)
		}
		rows = append(rows, annotationRow{
			PMID: pmid, JSON: string(encoded), Searchable: searchable.String(), Present: present,
		})
	}
	if len(rows) == 0 {
		return annotationCSV{}, errors.New("annotations CSV contains no data rows")
	}
	return annotationCSV{Columns: append([]string(nil), header...), Rows: rows}, nil
}

func completenessWarnings(columns []string, presentCounts map[string]int, total int) []AnnotationCompletenessWarning {
	if total == 0 {
		return []AnnotationCompletenessWarning{}
	}
	warnings := make([]AnnotationCompletenessWarning, 0)
	for _, field := range columns {
		completeness := float64(presentCounts[field]) / float64(total)
		if completeness >= annotationCompletenessThreshold || isAnnotationWarningException(field) {
			continue
		}
		warning := AnnotationCompletenessWarning{
			Field: field, PresentCount: presentCounts[field], TotalCount: total,
			Completeness: completeness,
		}
		warning.Message = annotationCompletenessMessage(warning)
		warnings = append(warnings, warning)
	}
	return warnings
}

func isAnnotationWarningException(field string) bool {
	_, exempt := annotationWarningExceptions[strings.ToLower(strings.TrimSpace(field))]
	return exempt
}

func annotationCompletenessMessage(warning AnnotationCompletenessWarning) string {
	return fmt.Sprintf(
		"%s is %.1f%% complete (%d/%d); results filtered by this field are not exhaustive",
		warning.Field, warning.Completeness*100, warning.PresentCount, warning.TotalCount,
	)
}

func validateAnnotationHeader(header []string) (int, error) {
	if len(header) == 0 {
		return 0, errors.New("annotations CSV has an empty header")
	}
	pmidColumn := -1
	seen := make(map[string]struct{}, len(header))
	for position := range header {
		header[position] = strings.TrimSpace(header[position])
		if header[position] == "" {
			return 0, errors.New("annotations CSV has an empty column name")
		}
		if _, duplicate := seen[header[position]]; duplicate {
			return 0, fmt.Errorf("annotations CSV has duplicate column %q", header[position])
		}
		seen[header[position]] = struct{}{}
		if header[position] == "pmid" {
			pmidColumn = position
		}
	}
	if pmidColumn < 0 {
		return 0, errors.New("annotations CSV is missing required column \"pmid\"")
	}
	return pmidColumn, nil
}

func convertAnnotationValue(name, raw string) (any, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if _, ok := annotationBooleanFields[name]; ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("invalid boolean %q", value)
		}
		return parsed, nil
	}
	if _, ok := annotationIntegerFields[name]; ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", value)
		}
		return parsed, nil
	}
	if _, ok := annotationFloatFields[name]; ok {
		parsed, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", value)
		}
		return parsed, nil
	}
	if _, ok := annotationJSONFields[name]; ok {
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return parsed, nil
	}
	if delimiter, ok := annotationListDelimiters[name]; ok {
		parts := strings.Split(value, delimiter)
		result := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result, nil
	}
	return value, nil
}

func appendAnnotationSearchValue(builder *strings.Builder, name string, value any) {
	if value == nil {
		return
	}
	var text string
	switch typed := value.(type) {
	case []string:
		text = strings.Join(typed, " | ")
	case map[string]any, []any:
		encoded, _ := json.Marshal(typed)
		text = string(encoded)
	default:
		text = fmt.Sprint(typed)
	}
	if builder.Len() > 0 {
		builder.WriteByte('\n')
	}
	fmt.Fprintf(builder, "%s: %s", name, text)
}
