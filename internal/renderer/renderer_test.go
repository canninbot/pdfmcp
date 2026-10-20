package renderer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/canninbot/pdfmcp/internal/testutil"
)

func TestFullPageAndCroppedRendering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pdf")
	testutil.WriteMinimalPDF(t, path)
	renderer := New()
	defer renderer.Close()
	full, err := renderer.Render(context.Background(), path, 1, nil, 72, 512)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.PNG) < 100 || full.Width != 200 || full.Height != 200 {
		t.Fatalf("unexpected full render: %+v, %d bytes", full, len(full.PNG))
	}
	region := Region{0.25, 0.25, 0.75, 0.75}
	crop, err := renderer.Render(context.Background(), path, 1, &region, 72, 256)
	if err != nil {
		t.Fatal(err)
	}
	if len(crop.PNG) < 100 || crop.Width > 256 || crop.Height > 256 || !crop.Cropped {
		t.Fatalf("unexpected crop render: %+v, %d bytes", crop, len(crop.PNG))
	}
}
