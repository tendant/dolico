package paddleocr

import (
	"context"
	"fmt"
	"sync"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
)

// VisionName is the engine identifier the service reports for Tier 3.
const VisionName = "mineru"

// VisionEngine is Tier 3: the fallback for pages the OCR tiers lose.
//
// It lives in this package, alongside the OCR client, because it is the same
// service on the same port — and, more importantly, the same worker pool.
// Sharing the OCR engine's semaphore is the point: a vision request occupies a
// worker exactly as an OCR request does, so giving Tier 3 its own concurrency
// budget would oversubscribe the service by however many workers it was
// allowed.
//
// It is never selected by the registry. The router calls it directly, for
// specific pages, after those pages have already failed the cheaper tiers.
type VisionEngine struct {
	ocr *Engine

	mu      sync.RWMutex
	version string
}

// NewVision builds the vision engine over an existing OCR client, sharing its
// HTTP client, timeout and concurrency limit.
//
// Returns nil when the service reports no vision tier, so a caller can treat
// "not installed" as "no Tier 3" rather than as an error to handle.
//
// The version starts out unknown rather than borrowing the OCR tier's. They
// are different models on different release cycles, and the version is part of
// the page cache key: labelling MinerU's output with PaddleOCR's version would
// make a MinerU upgrade invisible to the cache. The service cannot report it
// at startup either -- MinerU loads on first use -- so it is adopted from the
// first real answer.
func NewVision(ocr *Engine) *VisionEngine {
	if ocr == nil || !ocr.VisionAvailable() {
		return nil
	}
	return &VisionEngine{ocr: ocr}
}

func (e *VisionEngine) Name() string { return VisionName }

func (e *VisionEngine) Version() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.version == "" {
		return "unknown"
	}
	return e.version
}

// Inspect always declines: Tier 3 never decides what a document is.
func (e *VisionEngine) Inspect(context.Context, canonical.Source, string) (*engine.Inspection, error) {
	return nil, fmt.Errorf("%w: %s does not inspect documents", engine.ErrUnsupported, VisionName)
}

// Supports always scores zero — the router reaches this engine directly.
func (e *VisionEngine) Supports(*engine.Inspection) engine.SupportScore { return engine.SupportNone }

// Extract reads the named pages with the vision tier.
//
// Unlike the OCR engine this does not shard. Tier 3 fires on a handful of
// pages at most, and the service reads them one at a time regardless, so
// splitting the request would only multiply document uploads.
func (e *VisionEngine) Extract(ctx context.Context, req *engine.ExtractRequest) (*engine.ExtractResult, error) {
	if len(req.Pages) == 0 {
		return nil, fmt.Errorf("%w: %s extracts named pages only", engine.ErrUnsupported, VisionName)
	}

	select {
	case e.ocr.sem <- struct{}{}:
		defer func() { <-e.ocr.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	res, version, err := e.ocr.extractTier(ctx, req, req.Pages, "vision")
	if err != nil {
		return nil, err
	}
	if version != "" && version != e.Version() {
		e.mu.Lock()
		e.version = version
		e.mu.Unlock()
	}
	return res, nil
}

// VisionAvailable reports whether the service has the vision tier installed.
func (e *Engine) VisionAvailable() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.visionAvailable
}
