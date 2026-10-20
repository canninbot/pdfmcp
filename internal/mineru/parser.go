package mineru

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// ParseDirectory parses the best available MinerU structured output in dir.
func ParseDirectory(dir string) (Document, error) {
	structured := filepath.Join(dir, "structured_content.json")
	doc, err := ParseFile(structured)
	if err == nil && len(doc.Blocks) > 0 {
		mergeMiddleMetadata(dir, &doc)
		return doc, nil
	}
	firstErr := err
	middle := filepath.Join(dir, "middle_json.json")
	doc, middleErr := ParseFile(middle)
	if middleErr == nil && len(doc.Blocks) > 0 {
		if firstErr != nil {
			doc.Diagnostics = append(doc.Diagnostics, firstErr.Error())
		}
		return doc, nil
	}
	if firstErr != nil {
		return Document{}, firstErr
	}
	return doc, middleErr
}

// mergeMiddleMetadata preserves producer, document, and extraction extension
// data without adding middle_json blocks that would duplicate structured data.
func mergeMiddleMetadata(dir string, doc *Document) {
	path := filepath.Join(dir, "middle_json.json")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		doc.Diagnostics = append(doc.Diagnostics, fmt.Sprintf("read middle_json metadata: %v", err))
		return
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		doc.Diagnostics = append(doc.Diagnostics, "parse middle_json metadata: expected JSON object")
		return
	}
	var metadata map[string]any
	var extensions map[string]any
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			doc.Diagnostics = append(doc.Diagnostics, fmt.Sprintf("parse middle_json metadata: %v", err))
			return
		}
		key, ok := keyToken.(string)
		if !ok {
			doc.Diagnostics = append(doc.Diagnostics, "parse middle_json metadata: invalid object key")
			return
		}
		switch key {
		case "metadata":
			err = decoder.Decode(&metadata)
		case "extensions":
			err = decoder.Decode(&extensions)
		case "pages":
			if metadata != nil || extensions != nil {
				applyMiddleMetadata(doc, metadata, extensions)
				return
			}
			err = skipJSONValue(decoder)
		default:
			err = skipJSONValue(decoder)
		}
		if err != nil {
			doc.Diagnostics = append(doc.Diagnostics, fmt.Sprintf("parse middle_json metadata: %v", err))
			return
		}
	}
	applyMiddleMetadata(doc, metadata, extensions)
}

func applyMiddleMetadata(doc *Document, metadata, extensions map[string]any) {
	combined := make(map[string]any)
	if len(metadata) > 0 {
		combined["metadata"] = metadata
		if document, ok := metadata["document"].(map[string]any); ok {
			if doc.Title == "" {
				doc.Title = stringValue(document["title"])
			}
			if doc.PageCount == nil {
				if count, ok := intValue(document["page_count"]); ok {
					doc.PageCount = &count
				}
			}
		}
	}
	if len(extensions) > 0 {
		combined["extensions"] = extensions
	}
	if len(combined) == 0 {
		return
	}
	doc.ExtractionMetadata = combined
	if encoded, err := json.Marshal(combined); err == nil {
		doc.Metadata = encoded
	}
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	for decoder.More() {
		if delimiter == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// ParseFile parses alternate and partial MinerU JSON schemas without assuming
// a particular extraction version.
func ParseFile(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return Document{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	doc := Document{ExtractionMetadata: make(map[string]any)}
	if metadata, ok := root["metadata"].(map[string]any); ok {
		doc.ExtractionMetadata = metadata
		if encoded, marshalErr := json.Marshal(metadata); marshalErr == nil {
			doc.Metadata = encoded
		}
		if document, ok := metadata["document"].(map[string]any); ok {
			doc.Title = stringValue(document["title"])
			if count, ok := intValue(document["page_count"]); ok {
				doc.PageCount = &count
			}
		}
	}
	pages := findPages(root)
	if doc.PageCount == nil && len(pages) > 0 {
		count := len(pages)
		doc.PageCount = &count
	}
	section := ""
	foundDocumentTitle := false
	for pagePosition, pageValue := range pages {
		page, ok := pageValue.(map[string]any)
		if !ok {
			continue
		}
		pageIndex := pagePosition
		if value, found := firstInt(page, "page_idx", "page_index", "page_no", "page"); found {
			pageIndex = value
		}
		blocks := findBlockList(page)
		for blockPosition, blockValue := range blocks {
			blockMap, ok := blockValue.(map[string]any)
			if !ok {
				continue
			}
			block := canonicalBlock(blockMap, pageIndex, blockPosition, filepath.Base(path))
			if isHeading(block.Type) && block.Text != "" {
				section = block.Text
			}
			block.Section = section
			if block.Type == "doc_title" && !foundDocumentTitle {
				doc.Title = block.Text
				foundDocumentTitle = true
			}
			if block.Text != "" || block.Caption != "" || block.ImagePath != "" {
				doc.Blocks = append(doc.Blocks, block)
			}
		}
	}
	if len(doc.Blocks) == 0 {
		return doc, fmt.Errorf("%s has no usable page blocks", filepath.Base(path))
	}
	return doc, nil
}

// NormalizeBBox converts only explicitly recognizable coordinate systems.
func NormalizeBBox(values []float64, convention string, width, height float64) *BBox {
	if len(values) != 4 {
		return nil
	}
	box := &BBox{Raw: [4]float64{values[0], values[1], values[2], values[3]}}
	switch convention {
	case "normalized", "normalized_1":
		if validNormalized(box.Raw) {
			normalized := box.Raw
			box.Normalized = &normalized
			box.Convention = "normalized_1"
		}
	case "normalized_1000":
		normalized := [4]float64{values[0] / 1000, values[1] / 1000, values[2] / 1000, values[3] / 1000}
		if validNormalized(normalized) {
			box.Normalized = &normalized
			box.Convention = "normalized_1000"
		}
	case "pdf_points":
		if width > 0 && height > 0 {
			normalized := [4]float64{values[0] / width, values[1] / height, values[2] / width, values[3] / height}
			if validNormalized(normalized) {
				box.Normalized = &normalized
				box.Convention = "pdf_points"
			}
		}
	default:
		if validNormalized(box.Raw) {
			normalized := box.Raw
			box.Normalized = &normalized
			box.Convention = "normalized_1"
		} else {
			box.Convention = "unknown"
		}
	}
	return box
}

func validNormalized(box [4]float64) bool {
	return box[0] >= 0 && box[1] >= 0 && box[2] <= 1 && box[3] <= 1 &&
		box[0] < box[2] && box[1] < box[3]
}

func canonicalBlock(value map[string]any, pageIndex, position int, source string) Block {
	blockType := strings.ToLower(firstString(value, "type", "block_type", "category", "label"))
	if blockType == "" {
		blockType = "text"
	}
	id := firstString(value, "id", "block_id", "uid")
	if id == "" {
		if index, ok := firstInt(value, "index", "block_idx"); ok {
			id = fmt.Sprintf("p%d-b%d", pageIndex, index)
		} else {
			id = fmt.Sprintf("p%d-b%d", pageIndex, position)
		}
	}
	text := firstString(value, "text", "content", "html", "latex")
	caption := firstString(value, "caption", "img_caption", "table_caption")
	imagePath := firstString(value, "image_path", "img_path", "image_source", "image", "path")
	if text == "" {
		text = nestedText(value["content"])
	}
	if caption == "" {
		caption = nestedText(value["caption"])
	}
	if caption == "" {
		caption = nestedText(value["captions"])
	}
	if caption == "" && (strings.Contains(blockType, "figure") || strings.Contains(blockType, "image")) {
		caption = nestedTaggedText(value, "caption")
	}
	if footnotes := nestedText(value["footnotes"]); footnotes != "" {
		if caption == "" {
			caption = footnotes
		} else if !strings.Contains(caption, footnotes) {
			caption += "\n" + footnotes
		}
	}
	if imagePath == "" {
		imagePath = nestedImagePath(value)
	}
	var bbox *BBox
	if coordinates := floatSlice(value["bbox"]); len(coordinates) == 4 {
		convention := firstString(value, "bbox_convention", "coordinate_system", "bbox_type")
		bbox = NormalizeBBox(coordinates, convention, 0, 0)
	}
	page := pageIndex
	return Block{
		ID:          id,
		PageIndex:   &page,
		Type:        canonicalType(blockType),
		Text:        cleanText(text),
		Caption:     cleanText(caption),
		ImagePath:   filepath.ToSlash(imagePath),
		BBox:        bbox,
		Source:      source,
		SourceIndex: position,
	}
}

func findPages(root map[string]any) []any {
	for _, key := range []string{"pages", "page_list", "pdf_info"} {
		if values, ok := root[key].([]any); ok {
			return values
		}
	}
	for _, key := range []string{"document", "data", "result"} {
		if child, ok := root[key].(map[string]any); ok {
			if pages := findPages(child); len(pages) > 0 {
				return pages
			}
		}
	}
	return nil
}

func findBlockList(page map[string]any) []any {
	for _, key := range []string{"blocks", "content", "para_blocks", "layout_dets", "items"} {
		if blocks, ok := page[key].([]any); ok {
			return blocks
		}
	}
	return nil
}

func canonicalType(value string) string {
	switch {
	case value == "doc_title":
		return "doc_title"
	case strings.Contains(value, "table"):
		return "table"
	case strings.Contains(value, "figure"), strings.Contains(value, "image"), strings.Contains(value, "chart"):
		return "figure"
	case isHeading(value):
		return "heading"
	case value == "equation", value == "formula", value == "interline_equation":
		return "equation"
	default:
		return value
	}
}

func isHeading(value string) bool {
	return value == "title" || value == "doc_title" || value == "heading" ||
		value == "section" || strings.HasPrefix(value, "header")
}

func cleanText(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\x00' {
			return unicode.ReplacementChar
		}
		return r
	}, value))
}

func nestedText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, child := range typed {
			if text := nestedText(child); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"text", "content", "html", "latex"} {
			if text := nestedText(typed[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func nestedTaggedText(value map[string]any, tag string) string {
	for _, child := range value {
		array, ok := child.([]any)
		if !ok {
			continue
		}
		for _, item := range array {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			kind := strings.ToLower(firstString(entry, "type", "label", "category"))
			if strings.Contains(kind, tag) {
				return nestedText(entry)
			}
		}
	}
	return ""
}

func nestedImagePath(value any) string {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if path := nestedImagePath(child); path != "" {
				return path
			}
		}
	case map[string]any:
		for _, key := range []string{"image_path", "img_path", "image_source"} {
			if path := stringValue(typed[key]); path != "" {
				return path
			}
		}
		for _, child := range typed {
			if path := nestedImagePath(child); path != "" {
				return path
			}
		}
	}
	return ""
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result := stringValue(value[key]); result != "" {
			return result
		}
	}
	return ""
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	}
	return ""
}

func firstInt(value map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if result, ok := intValue(value[key]); ok {
			return result, true
		}
	}
	return 0, false
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), typed == float64(int(typed))
	case int:
		return typed, true
	case string:
		result, err := strconv.Atoi(typed)
		return result, err == nil
	}
	return 0, false
}

func floatSlice(value any) []float64 {
	array, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]float64, 0, len(array))
	for _, item := range array {
		number, ok := item.(float64)
		if !ok {
			return nil
		}
		result = append(result, number)
	}
	return result
}
