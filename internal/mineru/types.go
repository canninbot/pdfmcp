// Package mineru parses version-varying MinerU extraction output.
package mineru

import "encoding/json"

// BBox retains source coordinates and an optional canonical normalized box.
type BBox struct {
	Raw        [4]float64  `json:"raw"`
	Normalized *[4]float64 `json:"normalized,omitempty"`
	Convention string      `json:"convention"`
}

// Block is a canonical, searchable MinerU content block.
type Block struct {
	ID          string `json:"block_id"`
	PageIndex   *int   `json:"page_idx,omitempty"`
	Type        string `json:"type"`
	Section     string `json:"section,omitempty"`
	Text        string `json:"text,omitempty"`
	Caption     string `json:"caption,omitempty"`
	ImagePath   string `json:"image_path,omitempty"`
	BBox        *BBox  `json:"bbox,omitempty"`
	Source      string `json:"source"`
	SourceIndex int    `json:"source_index"`
}

// Document is the canonical subset of a MinerU extraction used by pdfmcp.
type Document struct {
	Title              string          `json:"title,omitempty"`
	PageCount          *int            `json:"page_count,omitempty"`
	Blocks             []Block         `json:"blocks"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	ExtractionMetadata map[string]any  `json:"extraction_metadata,omitempty"`
	Diagnostics        []string        `json:"diagnostics,omitempty"`
}
