// Package renderer provides lazy, CGO-free PDFium WebAssembly rendering.
package renderer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"math"
	"os"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

const (
	maxDPI         = 600
	maxRenderBytes = 25 << 20
)

// Region is a canonical normalized top-left-origin crop.
type Region [4]float64

// Output is one PNG page render.
type Output struct {
	PNG          []byte `json:"-"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	EffectiveDPI int    `json:"effective_dpi"`
	Cropped      bool   `json:"cropped"`
}

// Renderer owns a lazily initialized single PDFium WASM worker.
type Renderer struct {
	mu       sync.Mutex
	pool     pdfium.Pool
	instance pdfium.Pdfium
	closed   bool
}

// New creates an uninitialized renderer.
func New() *Renderer {
	return &Renderer{}
}

// Render renders a one-based page and optional normalized region.
func (r *Renderer) Render(ctx context.Context, path string, page int, region *Region, dpi, maxDimension int) (Output, error) {
	if page < 1 {
		return Output{}, errors.New("page must be one-based and positive")
	}
	if dpi <= 0 || dpi > maxDPI {
		return Output{}, fmt.Errorf("dpi must be between 1 and %d", maxDPI)
	}
	if maxDimension < 64 || maxDimension > 4096 {
		return Output{}, errors.New("max_dimension must be between 64 and 4096")
	}
	if region != nil {
		if err := validateRegion(*region); err != nil {
			return Output{}, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.initialize(); err != nil {
		return Output{}, err
	}
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Output{}, fmt.Errorf("open PDF: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Output{}, err
	}
	document, err := r.instance.OpenDocument(&requests.OpenDocument{
		FileReader: file, FileReaderSize: info.Size(),
	})
	if err != nil {
		return Output{}, fmt.Errorf("open PDFium document: %w", err)
	}
	defer r.instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: document.Document})
	pageCount, err := r.instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: document.Document})
	if err != nil {
		return Output{}, fmt.Errorf("get page count: %w", err)
	}
	if page > pageCount.PageCount {
		return Output{}, fmt.Errorf("page %d exceeds PDF page count %d", page, pageCount.PageCount)
	}
	pageRequest := requests.Page{ByIndex: &requests.PageByIndex{Document: document.Document, Index: page - 1}}
	size, err := r.instance.GetPageSize(&requests.GetPageSize{Page: pageRequest})
	if err != nil {
		return Output{}, fmt.Errorf("get page size: %w", err)
	}
	widthPoints, heightPoints := size.Width, size.Height
	var crop *requests.RenderPageCrop
	if region != nil {
		crop = &requests.RenderPageCrop{
			X: region[0] * size.Width, Y: region[1] * size.Height,
			Width:  (region[2] - region[0]) * size.Width,
			Height: (region[3] - region[1]) * size.Height,
		}
		widthPoints, heightPoints = crop.Width, crop.Height
		idealDPI := int(math.Floor(float64(maxDimension) * 72 / math.Max(widthPoints, heightPoints)))
		dpi = min(maxDPI, max(dpi, idealDPI))
	}
	predictedMax := math.Max(widthPoints, heightPoints) * float64(dpi) / 72
	if predictedMax > float64(maxDimension) {
		dpi = max(1, int(math.Floor(float64(maxDimension)*72/math.Max(widthPoints, heightPoints))))
	}
	rendered, err := r.instance.RenderPageInDPI(&requests.RenderPageInDPI{
		Page: pageRequest, DPI: dpi, Crop: crop,
	})
	if err != nil {
		return Output{}, fmt.Errorf("render page: %w", err)
	}
	defer rendered.Cleanup()
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	var output bytes.Buffer
	if err := png.Encode(&output, rendered.Result.RenderedImage); err != nil {
		return Output{}, fmt.Errorf("encode PNG: %w", err)
	}
	if output.Len() > maxRenderBytes {
		return Output{}, fmt.Errorf("rendered PNG exceeds %d bytes", maxRenderBytes)
	}
	return Output{
		PNG: output.Bytes(), Width: rendered.Result.Width, Height: rendered.Result.Height,
		EffectiveDPI: dpi, Cropped: region != nil,
	}, nil
}

// Close releases any initialized WASM instance and pool.
func (r *Renderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	var closeErr error
	if r.instance != nil {
		closeErr = r.instance.Close()
		r.instance = nil
	}
	if r.pool != nil {
		if err := r.pool.Close(); closeErr == nil {
			closeErr = err
		}
		r.pool = nil
	}
	return closeErr
}

func (r *Renderer) initialize() error {
	if r.closed {
		return errors.New("renderer is closed")
	}
	if r.instance != nil {
		return nil
	}
	pool, err := webassembly.Init(webassembly.Config{MinIdle: 0, MaxIdle: 1, MaxTotal: 1})
	if err != nil {
		return fmt.Errorf("initialize embedded PDFium WASM: %w", err)
	}
	instance, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		pool.Close()
		return fmt.Errorf("acquire PDFium WASM instance: %w", err)
	}
	r.pool = pool
	r.instance = instance
	return nil
}

func validateRegion(region Region) error {
	for _, coordinate := range region {
		if coordinate < 0 || coordinate > 1 {
			return errors.New("region coordinates must be normalized between 0 and 1")
		}
	}
	if region[0] >= region[2] || region[1] >= region[3] {
		return errors.New("region must satisfy x1 < x2 and y1 < y2")
	}
	return nil
}
