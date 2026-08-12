package imagedoc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/tendant/dolico/internal/cache"
	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/engine/imagedoc"
	"github.com/tendant/dolico/internal/engine/router"
)

// write puts bytes in a file with no extension, the way the blob store does:
// the engine must decide from content alone.
func write(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// tiny renders a 4x4 image in the given format, so the accept tests run
// against bytes a real encoder produced rather than a handwritten header.
func tiny(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.Black)
	var buf bytes.Buffer
	var err error
	switch format {
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "png":
		err = png.Encode(&buf, img)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	default:
		t.Fatalf("no encoder for %q", format)
	}
	if err != nil {
		t.Fatalf("encode %s: %v", format, err)
	}
	return buf.Bytes()
}

func TestInspectClaimsImages(t *testing.T) {
	// TIFF, BMP and WebP have no encoder in the standard library, so those
	// cases are the signature bytes plus enough padding to reach the sniff
	// length -- which is all the detector reads anyway.
	pad := func(head []byte) []byte { return append(head, make([]byte, 64)...) }
	cases := []struct {
		name string
		data []byte
	}{
		{"jpeg", tiny(t, "jpeg")},
		{"png", tiny(t, "png")},
		{"gif", tiny(t, "gif")},
		{"tiff-little-endian", pad([]byte{'I', 'I', 0x2A, 0x00})},
		{"tiff-big-endian", pad([]byte{'M', 'M', 0x00, 0x2A})},
		{"webp", pad([]byte("RIFF\x00\x00\x00\x00WEBP"))},
		{"bmp", pad([]byte{'B', 'M', 0x10, 0, 0, 0, 0, 0, 0, 0})},
	}

	e := imagedoc.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := canonical.Source{Filename: "photo", MediaType: "application/octet-stream"}
			insp, err := e.Inspect(context.Background(), src, write(t, tc.data))
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if insp.Engine != imagedoc.Name {
				t.Errorf("engine = %q, want %q", insp.Engine, imagedoc.Name)
			}
			if insp.PageCount != 1 || len(insp.Pages) != 1 {
				t.Fatalf("page count = %d, pages = %d, want 1 and 1", insp.PageCount, len(insp.Pages))
			}
			page := insp.Pages[0]
			if page.Number != 1 {
				t.Errorf("page number = %d, want 1", page.Number)
			}
			// Scanned is what sends the page to the OCR tier. Any other
			// classification routes it to a native extractor that cannot read
			// pixels, which is the bug this engine exists to fix.
			if page.Classification.Type != canonical.PageTypeScanned {
				t.Errorf("classification = %q, want %q", page.Classification.Type, canonical.PageTypeScanned)
			}
			if got := e.Supports(insp); got != engine.SupportNative {
				t.Errorf("Supports = %d, want %d", got, engine.SupportNative)
			}
		})
	}
}

func TestInspectDeclinesEverythingElse(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"pdf", []byte("%PDF-1.7\n1 0 obj\n")},
		{"plain text", []byte("Dear Sir or Madam,\n\nyours faithfully.\n")},
		// "BM" is two bytes, and a text file may well start with them. The
		// reserved fields are what tells them apart.
		{"text starting BM", []byte("BMW service record for the fleet, 2026.\n")},
		// Images Pillow cannot open without extra codecs. Declining here is
		// what makes the API's "no reader for this format" answer true.
		{"svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"heic", []byte("\x00\x00\x00\x20ftypheic\x00\x00\x00\x00heicmif1")},
		{"empty", nil},
		{"shorter than any signature", []byte{0xFF}},
	}

	e := imagedoc.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := canonical.Source{Filename: "file"}
			_, err := e.Inspect(context.Background(), src, write(t, tc.data))
			if !errors.Is(err, engine.ErrUnsupported) {
				t.Fatalf("error = %v, want ErrUnsupported", err)
			}
		})
	}
}

// A declared media type is a claim, not a fact: bytes that are not an image
// must be declined however they were labeled.
func TestInspectIgnoresDeclaredMediaType(t *testing.T) {
	e := imagedoc.New()
	src := canonical.Source{Filename: "invoice.jpg", MediaType: "image/jpeg"}
	if _, err := e.Inspect(context.Background(), src, write(t, []byte("%PDF-1.7\n"))); !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestSupportsDeclinesOtherEnginesInspections(t *testing.T) {
	e := imagedoc.New()
	if got := e.Supports(&engine.Inspection{Engine: "pdf"}); got != engine.SupportNone {
		t.Errorf("Supports(pdf inspection) = %d, want %d", got, engine.SupportNone)
	}
	if got := e.Supports(nil); got != engine.SupportNone {
		t.Errorf("Supports(nil) = %d, want %d", got, engine.SupportNone)
	}
}

func TestExtractDeclines(t *testing.T) {
	e := imagedoc.New()
	_, err := e.Extract(context.Background(), &engine.ExtractRequest{Pages: []int{1}})
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

// fakeOCR stands in for the OCR tier: it records the pages it was asked for
// and returns text for them, which is all the routing assertion needs.
type fakeOCR struct{ calls [][]int }

func (f *fakeOCR) Name() string    { return "fake-ocr" }
func (f *fakeOCR) Version() string { return "1" }

func (f *fakeOCR) Inspect(context.Context, canonical.Source, string) (*engine.Inspection, error) {
	return nil, fmt.Errorf("%w: the OCR tier does not inspect", engine.ErrUnsupported)
}

func (f *fakeOCR) Supports(*engine.Inspection) engine.SupportScore { return engine.SupportNone }

func (f *fakeOCR) Extract(_ context.Context, req *engine.ExtractRequest) (*engine.ExtractResult, error) {
	f.calls = append(f.calls, append([]int(nil), req.Pages...))
	pages := make([]canonical.Page, 0, len(req.Pages))
	for _, n := range req.Pages {
		conf := 0.95
		pages = append(pages, canonical.Page{
			Number:         n,
			Kind:           canonical.PageKindPaginated,
			Classification: canonical.Classification{Type: canonical.PageTypeScanned, Confidence: 1},
			Blocks: []canonical.Block{{
				ID:         fmt.Sprintf("p%d-ocr0", n),
				Type:       canonical.BlockParagraph,
				Text:       "The text this photograph contains, read by the OCR tier and long enough to score.",
				Confidence: &conf,
				Provenance: canonical.Provenance{Engine: "fake-ocr", EngineVersion: "1", Method: "ocr"},
			}},
		})
	}
	return &engine.ExtractResult{Pages: pages}, nil
}

// The end this whole engine exists for: an uploaded JPEG produces a document
// instead of "unsupported", and the page comes from the OCR tier.
func TestRouterSendsAnImageToOCR(t *testing.T) {
	path := write(t, tiny(t, "jpeg"))
	ocr := &fakeOCR{}
	// imagedoc.Extract returns an error, so a router that wrongly tried to
	// extract natively would fail this test rather than quietly return an
	// empty page.
	rt := router.New(
		engine.NewRegistry(imagedoc.New(), ocr),
		ocr,
		cache.New(0),
		router.Options{OCRThreshold: 0.6, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)

	doc, err := rt.Process(context.Background(), router.Request{
		DocumentID: "doc-1",
		TraceID:    "trace-1",
		Source:     canonical.Source{Filename: "Weixin Image_20260728214536_133_4389.jpg", MediaType: "image/jpeg"},
		Path:       path,
		AssetsDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(doc.Pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(doc.Pages))
	}
	if len(ocr.calls) != 1 || len(ocr.calls[0]) != 1 || ocr.calls[0][0] != 1 {
		t.Fatalf("OCR calls = %v, want one call for page 1", ocr.calls)
	}
	if len(doc.Pages[0].Blocks) == 0 {
		t.Fatal("page has no blocks; the OCR result was not merged in")
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != "fake-ocr" {
		t.Errorf("block engine = %q, want fake-ocr", got)
	}
}
