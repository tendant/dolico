// Package imagedoc is the inspector for standalone images.
//
// A photograph or a scan saved as JPEG is a document with exactly one page and
// no text layer at all. Nothing else in the registry claims it: the Rust shim
// owns PDFs and the native formats, and the OCR tier never decides what a
// document is. So an image reached the router, found no taker, and came back
// as "unsupported" -- even though the OCR service has read standalone images
// all along.
//
// This engine closes that gap and does nothing else. It inspects, classifies
// the single page as scanned, and steps aside: the router's existing partition
// sends a scanned page straight to the OCR tier, which posts the original
// bytes to the service, which wraps them as one page. There is no extraction
// path here to write, and deliberately so -- Extract declines.
package imagedoc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
)

// Name is the engine identifier recorded in the trace.
const Name = "image"

// Version identifies this inspector's build. It participates in cache keys,
// though only through inspection: the pages themselves are produced and
// versioned by the OCR tier.
const Version = "1"

// ReasonStandaloneImage explains the classification: the page is not scanned
// in the sense a PDF page is scanned -- there was never a text layer to lose.
const ReasonStandaloneImage = "standalone_image"

// sniffLen is how many bytes the format check needs. The longest signature is
// WebP's, at 12.
const sniffLen = 32

// Engine inspects standalone images.
type Engine struct{}

// New returns the image inspector.
func New() *Engine { return &Engine{} }

func (e *Engine) Name() string    { return Name }
func (e *Engine) Version() string { return Version }

// Inspect claims the file if its bytes are an image the OCR service can
// decode, and declines otherwise.
//
// Detection is by content, not by name or by the declared media type: blobs
// are stored under their digest, so the path carries no extension, and a
// browser's Content-Type is a claim rather than a fact. Declaring
// "image/jpeg" over bytes that are not a JPEG must fail here, at the cheap
// step, rather than several seconds later inside the OCR service.
func (e *Engine) Inspect(_ context.Context, src canonical.Source, path string) (*engine.Inspection, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read the document: %w", Name, err)
	}
	defer f.Close()

	head := make([]byte, sniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: cannot read the document: %w", Name, err)
	}
	format, ok := detect(head[:n])
	if !ok {
		return nil, fmt.Errorf("%w: %s is not an image %s reads", engine.ErrUnsupported, src.Filename, Name)
	}

	return &engine.Inspection{
		Source:    src,
		PageCount: 1,
		PageKind:  canonical.PageKindPaginated,
		Pages: []canonical.Page{{
			Number: 1,
			Kind:   canonical.PageKindPaginated,
			Classification: canonical.Classification{
				Type: canonical.PageTypeScanned,
				// Certain, unlike a PDF page where "scanned" is inferred from
				// the absence of text operators and can be wrong.
				Confidence: 1,
				Reasons:    []string{ReasonStandaloneImage},
			},
		}},
		Metadata: canonical.Metadata{
			PageCount: 1,
			Custom:    map[string]string{"image_format": format},
		},
		Engine: Name,
	}, nil
}

// Supports scores native for the inspections this engine produced.
func (e *Engine) Supports(insp *engine.Inspection) engine.SupportScore {
	if insp != nil && insp.Engine == Name {
		return engine.SupportNative
	}
	return engine.SupportNone
}

// Extract always declines: an image has no text to extract cheaply, and the
// router routes its single scanned page to the OCR tier without ever asking
// this engine. Reaching here means the routing rules changed underneath, which
// should be loud rather than answered with an empty page.
func (e *Engine) Extract(context.Context, *engine.ExtractRequest) (*engine.ExtractResult, error) {
	return nil, fmt.Errorf("%w: %s does not extract; image pages are read by the OCR tier",
		engine.ErrUnsupported, Name)
}

// detect names the image format of the given leading bytes, or reports false.
//
// The accepted set is exactly what the OCR service can decode with Pillow.
// SVG, HEIC and AVIF are images that Pillow will not open without extra
// codecs, so they are declined here and produce the honest "no reader for this
// format" answer instead of an OCR failure several seconds in.
func detect(head []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg", true
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "png", true
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "gif", true
	case bytes.HasPrefix(head, []byte{'I', 'I', 0x2A, 0x00}), bytes.HasPrefix(head, []byte{'M', 'M', 0x00, 0x2A}):
		return "tiff", true
	case len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "webp", true
	case isBMP(head):
		return "bmp", true
	}
	return "", false
}

// isBMP recognizes a Windows bitmap.
//
// "BM" alone is two bytes and would claim any text file starting with those
// letters, so the check also requires the two reserved fields to be zero, as
// the format specifies, and a header long enough to hold them.
func isBMP(head []byte) bool {
	if len(head) < 26 || head[0] != 'B' || head[1] != 'M' {
		return false
	}
	return bytes.Equal(head[6:10], []byte{0, 0, 0, 0})
}
