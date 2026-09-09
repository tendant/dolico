package paddleocr

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
)

// DefaultVisionName is the Tier 3 engine to assume before the service has said
// which one it serves.
//
// It is a default rather than the answer. The tier has two engines -- MinerU
// and GLM-OCR -- selected by DOLICO_VISION_ENGINE on the service, and the name
// feeds both Provenance.Engine and the page cache key. Hardcoding one of them
// would file the other's pages under it, so a switch of engines would serve
// cached pages the new engine never produced.
const DefaultVisionName = "mineru"

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
	name    string
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
// at startup either -- the vision model loads on first use -- so it is adopted
// from the first real answer.
//
// The name is taken from the service at startup, where the version cannot be:
// which engine is in the Tier 3 slot is a configuration the service knows
// before it loads anything, and Name() is read for logs and errors long before
// a page is escalated. It is re-adopted from the first real answer as well, in
// case the service was reconfigured underneath us.
func NewVision(ocr *Engine) *VisionEngine {
	if ocr == nil || !ocr.VisionAvailable() {
		return nil
	}
	return &VisionEngine{ocr: ocr, name: ocr.VisionEngineName()}
}

func (e *VisionEngine) Name() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.name == "" {
		return DefaultVisionName
	}
	return e.name
}

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
	return nil, fmt.Errorf("%w: %s does not inspect documents", engine.ErrUnsupported, e.Name())
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
		return nil, fmt.Errorf("%w: %s extracts named pages only", engine.ErrUnsupported, e.Name())
	}

	select {
	case e.ocr.sem <- struct{}{}:
		defer func() { <-e.ocr.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	res, who, err := e.ocr.extractTier(ctx, req, req.Pages, "vision")
	if err != nil {
		// A failed call teaches nothing about who answered, so the name this
		// engine is carrying stays whatever startup found -- and if the
		// service has been reconfigured since, every log line about the
		// failure names the wrong engine. That is exactly how a MinerU tier
		// came to report "escalating to vision engine=glm-ocr" while it
		// downloaded MinerU's weights.
		//
		// So re-read it, in the background: the caller gets its error now, and
		// the next attempt is described correctly.
		go e.refreshName()
		return nil, err
	}
	e.adopt(who)
	return res, nil
}

// refreshName re-reads which engine the service has in its Tier 3 slot.
//
// Its own context, not the caller's: this runs after a request failed, and
// that request's context is usually already cancelled or past its deadline.
func (e *VisionEngine) refreshName() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.ocr.refreshVersion(ctx); err != nil {
		return
	}
	if name := e.ocr.VisionEngineName(); name != "" {
		e.mu.Lock()
		e.name = name
		e.mu.Unlock()
	}
}

// adopt records who actually answered.
//
// Both fields feed the page cache key, and neither can be known before a page
// has been read: the version because the model loads lazily, the name because
// the service may have been reconfigured since startup. Taking them from the
// answer rather than asserting them is what keeps a cached page attributable
// to the engine that produced it.
func (e *VisionEngine) adopt(who tierIdentity) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if who.Version != "" {
		e.version = who.Version
	}
	if who.Engine != "" {
		e.name = who.Engine
	}
}

// VisionAvailable reports whether the service has the vision tier installed.
func (e *Engine) VisionAvailable() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.visionAvailable
}

// VisionEngineName is which engine the service has in its Tier 3 slot, or ""
// if it did not say.
func (e *Engine) VisionEngineName() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.visionEngine
}
