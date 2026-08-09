// Package ocrstub is a placeholder OCR engine.
//
// It implements the real Engine interface and is wired into the real router,
// so the whole escalation path -- classify, route, extract, score, escalate,
// merge, record provenance -- is exercised end to end. What it does not do is
// read pixels: it returns one synthetic block per page, marked with low
// confidence and unmistakable text.
//
// The point is that replacing it is a swap, not a rewrite. A PaddleOCR service
// implements these same three methods over HTTP and the router does not
// change. Two things it will need that this stub does not: page rasterization
// (neither pdf-inspector nor anydoc renders, so the OCR service should
// rasterize with pypdfium2 from the original bytes plus the page numbers) and
// real per-block confidences.
package ocrstub

import (
	"context"
	"fmt"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
)

// Name is the engine identifier recorded in block provenance. It is
// deliberately not "paddleocr": a document processed by the stub must never be
// mistaken for one that saw a real OCR engine.
const Name = "ocr-stub"

// Version identifies the stub build. It participates in cache keys, so
// swapping in a real engine invalidates every page this one produced.
const Version = "0-stub"

// Confidence is what the stub reports for the blocks it invents. It is low on
// purpose: anything that ships to production while still consuming stub output
// should look obviously wrong in the data.
const Confidence = 0.10

// Engine is the stub OCR engine.
type Engine struct{}

// New returns the stub engine.
func New() *Engine { return &Engine{} }

func (e *Engine) Name() string    { return Name }
func (e *Engine) Version() string { return Version }

// Inspect always declines. The OCR tier is never the engine that decides what
// a document is; the router hands it specific pages after another engine has
// classified them.
func (e *Engine) Inspect(context.Context, canonical.Source, string) (*engine.Inspection, error) {
	return nil, fmt.Errorf("%w: %s does not inspect documents", engine.ErrUnsupported, Name)
}

// Supports always scores zero, for the same reason.
func (e *Engine) Supports(*engine.Inspection) engine.SupportScore { return engine.SupportNone }

// Extract returns one synthetic block per requested page.
func (e *Engine) Extract(_ context.Context, req *engine.ExtractRequest) (*engine.ExtractResult, error) {
	if len(req.Pages) == 0 {
		return nil, fmt.Errorf("%w: %s extracts named pages only", engine.ErrUnsupported, Name)
	}

	conf := Confidence
	pages := make([]canonical.Page, 0, len(req.Pages))
	for _, number := range req.Pages {
		prov := canonical.Provenance{
			Engine:        Name,
			EngineVersion: Version,
			Method:        "ocr-stub/synthetic",
		}
		pages = append(pages, canonical.Page{
			Number: number,
			Kind:   canonical.PageKindPaginated,
			Classification: canonical.Classification{
				Type:       canonical.PageTypeScanned,
				Confidence: conf,
				Reasons:    []string{"ocr_stub"},
			},
			Blocks: []canonical.Block{{
				ID:   fmt.Sprintf("p%d-ocr0", number),
				Type: canonical.BlockParagraph,
				Text: fmt.Sprintf(
					"[ocr-stub] page %d of %s was routed to OCR and not actually read. "+
						"Configure a real OCR engine to extract this page.",
					number, req.Source.Filename),
				Confidence: &conf,
				Provenance: prov,
			}},
		})
	}
	return &engine.ExtractResult{Pages: pages}, nil
}
