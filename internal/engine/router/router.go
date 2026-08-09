// Package router turns an uploaded document into a canonical document.
//
// This is where the design's central rule lives: never OCR an entire document
// simply because it is a PDF. A document is inspected once, its pages are
// classified individually, and each page goes to the cheapest engine that can
// handle it. A two-page PDF with one text page and one scan costs one OCR
// call, not two, and not zero.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/tendant/dolico/internal/cache"
	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/engine/quality"
)

// Options configures routing behavior.
type Options struct {
	// OCRThreshold is the page quality score below which a text-extracted page
	// is re-extracted by the OCR tier.
	OCRThreshold float64
	// VisionThreshold is the second, lower bar: a page whose *OCR* result
	// still scores below this is escalated again, to the vision tier. It sits
	// below OCRThreshold on purpose — a page at 0.5 is mediocre but readable,
	// a page at 0.2 is garbage, and garbage is what Tier 3 is for.
	VisionThreshold float64
	// VisionMaxPages bounds how many pages of one document may reach the
	// vision tier. Tier 3 costs seconds per page, so a wholly unreadable
	// document must not turn into an unbounded run.
	VisionMaxPages int
	// Weights tunes the quality scorer.
	Weights quality.Weights
	// Logger receives routing decisions. Required.
	Logger *slog.Logger
}

// Router executes the pipeline for one document at a time.
type Router struct {
	registry *engine.Registry
	ocr      engine.Engine
	vision   engine.Engine
	cache    *cache.Cache
	opts     Options
}

// New builds a router. ocr may be nil, in which case pages needing OCR are
// kept with their classification and an empty block list rather than failing
// the document.
func New(reg *engine.Registry, ocr engine.Engine, c *cache.Cache, opts Options) *Router {
	if opts.Weights == (quality.Weights{}) {
		opts.Weights = quality.DefaultWeights
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.VisionMaxPages <= 0 {
		opts.VisionMaxPages = 5
	}
	return &Router{registry: reg, ocr: ocr, cache: c, opts: opts}
}

// WithVision attaches the vision tier. A nil engine leaves the router at two
// tiers, which is the default: Tier 3 is opt-in.
func (r *Router) WithVision(v engine.Engine) *Router {
	if v != nil {
		r.vision = v
	}
	return r
}

// Request is one document to process.
type Request struct {
	DocumentID string
	TraceID    string
	Source     canonical.Source
	// Path is the document's location in the blob store.
	Path string
	// AssetsDir is where engines write extracted assets.
	AssetsDir string
}

// Process runs the pipeline: inspect, route per page, extract, score,
// escalate, assemble.
func (r *Router) Process(ctx context.Context, req Request) (*canonical.Document, error) {
	log := r.opts.Logger.With("trace_id", req.TraceID, "document_id", req.DocumentID)

	doc := &canonical.Document{
		SchemaVersion: canonical.SchemaVersion,
		ID:            req.DocumentID,
		Source:        req.Source,
		Trace: canonical.Trace{
			TraceID:         req.TraceID,
			PipelineVersion: canonical.PipelineVersion,
		},
	}

	// 1. Inspect: one cheap look, and the engine that will do the primary
	//    extraction falls out of it.
	started := time.Now()
	insp, primary, err := r.registry.Inspect(ctx, req.Source, req.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	doc.Trace.Engines = append(doc.Trace.Engines, canonical.EngineRun{
		Engine:     primary.Name() + ":inspect",
		Version:    primary.Version(),
		DurationMS: time.Since(started).Milliseconds(),
	})
	doc.Metadata = insp.Metadata
	log.Info("inspected",
		"engine", primary.Name(),
		"pages", insp.PageCount,
		"page_kind", insp.PageKind)

	// 2. Route: split the pages by which tier should extract them.
	native, ocrPages := partition(insp)
	log.Info("routed",
		"native_pages", len(native),
		"ocr_pages", len(ocrPages),
		"ocr_page_numbers", ocrPages)

	byNumber := make(map[int]canonical.Page, insp.PageCount)

	// 3. Extract natively. An inspection with no per-page detail means the
	//    format has no pagination to route on, so the whole document goes to
	//    the primary engine as one unit.
	if len(native) > 0 || len(insp.Pages) == 0 {
		res, err := r.extract(ctx, primary, req, native, doc)
		if err != nil {
			return nil, fmt.Errorf("extract with %s: %w", primary.Name(), err)
		}
		mergeMetadata(&doc.Metadata, res.Metadata)
		doc.Assets = append(doc.Assets, res.Assets...)
		for _, p := range res.Pages {
			byNumber[p.Number] = p
		}
	}

	// 4. Score every natively-extracted page, and collect the ones whose
	//    output does not hold up. A page can extract "successfully" and be
	//    garbage -- broken font encodings are the common case -- so the
	//    engine's own verdict is not the last word.
	var escalate []int
	for _, number := range native {
		page, ok := byNumber[number]
		if !ok {
			continue
		}
		q := quality.Score(&page, r.opts.Weights)
		page.Quality = &q
		byNumber[number] = page
		if q.Score < r.opts.OCRThreshold {
			escalate = append(escalate, number)
			log.Info("escalating page",
				"page", number,
				"score", q.Score,
				"threshold", r.opts.OCRThreshold,
				"chars", q.CharCount,
				"replacement_ratio", q.ReplacementRatio)
		}
	}

	// 5. OCR: the pages classified as needing it, plus the ones that scored
	//    badly.
	todo := union(ocrPages, escalate)
	if len(todo) > 0 {
		if r.ocr == nil {
			log.Warn("pages need OCR but no OCR engine is configured", "pages", todo)
			for _, number := range todo {
				if _, ok := byNumber[number]; !ok {
					byNumber[number] = placeholderPage(number, insp, "no_ocr_engine")
				}
			}
		} else {
			res, err := r.extract(ctx, r.ocr, req, todo, doc)
			switch {
			case err != nil:
				// A failed OCR tier must not discard the pages that extracted
				// fine. The document is returned with those pages intact and
				// the failure recorded in the trace.
				log.Error("ocr extraction failed", "pages", todo, "error", err)
				for _, number := range todo {
					if _, ok := byNumber[number]; !ok {
						byNumber[number] = placeholderPage(number, insp, "ocr_failed")
					}
				}
			default:
				escalated := intSet(escalate)
				for _, p := range res.Pages {
					// Score what OCR produced, not what the previous engine
					// did. Carrying the old score forward would describe text
					// that is no longer on the page -- and it is the number
					// the vision tier reads when deciding what to escalate,
					// so a stale one sends Tier 3 at the wrong pages.
					q := quality.Score(&p, r.opts.Weights)
					q.Escalated = escalated[p.Number]
					p.Quality = &q
					byNumber[p.Number] = p
				}
				doc.Assets = append(doc.Assets, res.Assets...)
			}
		}
	}

	// 5b. Escalate again, to the vision tier, for pages the OCR tier still did
	//     badly on. This is the same confidence-driven mechanism as step 4,
	//     one tier down: score what OCR produced and re-run the worst of it
	//     with an engine of a different kind.
	r.escalateToVision(ctx, req, todo, byNumber, doc, log)

	// 6. Assemble in page order, filling any gap rather than silently
	//    returning a document that is missing a page.
	doc.Pages = make([]canonical.Page, 0, len(byNumber))
	if insp.PageCount > 0 {
		for number := 1; number <= insp.PageCount; number++ {
			page, ok := byNumber[number]
			if !ok {
				log.Warn("no engine produced this page", "page", number)
				page = placeholderPage(number, insp, "not_extracted")
			}
			if page.Quality == nil {
				q := quality.Score(&page, r.opts.Weights)
				page.Quality = &q
			}
			doc.Pages = append(doc.Pages, page)
		}
	} else {
		for _, p := range byNumber {
			doc.Pages = append(doc.Pages, p)
		}
		sort.Slice(doc.Pages, func(i, j int) bool { return doc.Pages[i].Number < doc.Pages[j].Number })
	}
	doc.Metadata.PageCount = len(doc.Pages)
	return doc, nil
}

// escalateToVision re-reads OCR pages that still scored badly.
//
// The governing rule is that Tier 3 must never make a page worse: a vision
// result replaces the OCR one only when the call succeeds, and any failure
// leaves the OCR result exactly as it was, with the reason recorded on the
// page so the outcome is visible rather than silent.
// ocrPages is exactly the set the OCR tier was asked for, passed in rather
// than inferred from block provenance: the router already knows, and matching
// on engine names would silently stop working the day a tier is renamed.
func (r *Router) escalateToVision(
	ctx context.Context,
	req Request,
	ocrPages []int,
	byNumber map[int]canonical.Page,
	doc *canonical.Document,
	log *slog.Logger,
) {
	if r.vision == nil || r.opts.VisionThreshold <= 0 || len(ocrPages) == 0 {
		return
	}

	type candidate struct {
		number int
		score  float64
	}
	var candidates []candidate
	for _, number := range ocrPages {
		page, ok := byNumber[number]
		if !ok {
			continue
		}
		// A page that went straight to OCR has not been scored yet — only
		// natively-extracted pages were scored in step 4 — so score it here
		// rather than skipping it, which would make the commonest candidate
		// (a scanned page) unreachable.
		if page.Quality == nil {
			q := quality.Score(&page, r.opts.Weights)
			page.Quality = &q
			byNumber[number] = page
		}
		if page.Quality.Score < r.opts.VisionThreshold {
			candidates = append(candidates, candidate{number, page.Quality.Score})
		}
	}
	if len(candidates) == 0 {
		return
	}

	// Worst first, so that a cap keeps the pages that need it most.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		return candidates[i].number < candidates[j].number
	})
	dropped := 0
	if len(candidates) > r.opts.VisionMaxPages {
		dropped = len(candidates) - r.opts.VisionMaxPages
		candidates = candidates[:r.opts.VisionMaxPages]
	}

	pages := make([]int, len(candidates))
	for i, c := range candidates {
		pages[i] = c.number
	}
	sort.Ints(pages)

	log.Info("escalating to vision",
		"pages", pages,
		"threshold", r.opts.VisionThreshold,
		"engine", r.vision.Name())
	if dropped > 0 {
		// Never truncate silently: a document where most pages needed Tier 3
		// and only some got it is a materially different result.
		log.Warn("vision page cap reached; some pages keep their OCR result",
			"cap", r.opts.VisionMaxPages, "dropped", dropped)
	}

	res, err := r.extract(ctx, r.vision, req, pages, doc)
	if err != nil {
		log.Error("vision extraction failed; keeping the OCR results", "pages", pages, "error", err)
		for _, number := range pages {
			markPage(byNumber, number, "vision_failed")
		}
		return
	}

	returned := make(map[int]bool, len(res.Pages))
	for _, page := range res.Pages {
		prev, ok := byNumber[page.Number]
		if !ok {
			continue
		}
		if len(page.Blocks) == 0 {
			// An empty vision result is worse than what we already have.
			markPage(byNumber, page.Number, "vision_empty")
			continue
		}
		returned[page.Number] = true
		// Re-score: the page is different text now, and the quality attached
		// to it must describe what is actually there.
		q := quality.Score(&page, r.opts.Weights)
		q.Escalated = true
		page.Quality = &q
		page.Classification.Reasons = append(page.Classification.Reasons, "vision_escalated")
		log.Info("vision replaced an OCR page",
			"page", page.Number, "was", prev.Quality.Score, "now", q.Score)
		byNumber[page.Number] = page
	}

	// Pages the tier simply did not answer for keep their OCR result too.
	for _, number := range pages {
		if !returned[number] {
			markPage(byNumber, number, "vision_failed")
		}
	}
}

// markPage appends a reason to a page without disturbing anything else on it.
func markPage(byNumber map[int]canonical.Page, number int, reason string) {
	page, ok := byNumber[number]
	if !ok {
		return
	}
	page.Classification.Reasons = append(
		append([]string(nil), page.Classification.Reasons...), reason)
	byNumber[number] = page
}

// extract runs one engine over the given pages, consulting the page cache
// first and recording the run in the document trace.
func (r *Router) extract(
	ctx context.Context,
	eng engine.Engine,
	req Request,
	pages []int,
	doc *canonical.Document,
) (*engine.ExtractResult, error) {
	result := &engine.ExtractResult{}

	// The cache is keyed per page, so an engine upgrade re-runs only what that
	// engine produced and a partial re-run costs only the missing pages.
	var missing []int
	if r.cache != nil {
		for _, number := range pages {
			key := cache.Key{
				DocumentHash:  req.Source.SHA256,
				Engine:        eng.Name(),
				EngineVersion: eng.Version(),
				Page:          number,
			}
			if page, ok := r.cache.Get(key); ok {
				result.Pages = append(result.Pages, page)
				continue
			}
			missing = append(missing, number)
		}
		if len(missing) == 0 && len(pages) > 0 {
			// Assets belong to the document, not to any one page, so they have
			// to come back on a page-cache hit too. Without this the rerun
			// produces an asset-less document and overwrites the stored one.
			if assets, ok := r.cache.GetAssets(assetCacheKey(req, eng)); ok {
				result.Assets = assets
			}
			doc.Trace.Engines = append(doc.Trace.Engines, canonical.EngineRun{
				Engine: eng.Name(), Version: eng.Version(), Pages: pages, CacheHit: true,
			})
			return result, nil
		}
	} else {
		missing = pages
	}

	started := time.Now()
	res, err := eng.Extract(ctx, &engine.ExtractRequest{
		Source:    req.Source,
		Path:      req.Path,
		Pages:     missing,
		AssetsDir: req.AssetsDir,
	})
	run := canonical.EngineRun{
		Engine:     eng.Name(),
		Version:    eng.Version(),
		Pages:      missing,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if err != nil {
		run.Error = err.Error()
		doc.Trace.Engines = append(doc.Trace.Engines, run)
		return nil, err
	}
	doc.Trace.Engines = append(doc.Trace.Engines, run)

	if r.cache != nil {
		for _, page := range res.Pages {
			r.cache.Put(cache.Key{
				DocumentHash:  req.Source.SHA256,
				Engine:        eng.Name(),
				EngineVersion: eng.Version(),
				Page:          page.Number,
			}, page)
		}
		// Recorded even when empty, so a later hit can distinguish "this
		// engine produced no assets" from "nothing is cached".
		r.cache.PutAssets(assetCacheKey(req, eng), res.Assets)
	}

	result.Pages = append(result.Pages, res.Pages...)
	result.Assets = res.Assets
	result.Metadata = res.Metadata
	sort.Slice(result.Pages, func(i, j int) bool { return result.Pages[i].Number < result.Pages[j].Number })
	return result, nil
}

func assetCacheKey(req Request, eng engine.Engine) cache.Key {
	return cache.Key{
		DocumentHash:  req.Source.SHA256,
		Engine:        eng.Name(),
		EngineVersion: eng.Version(),
	}
}

// partition splits inspected pages into those the primary engine should
// extract and those that go straight to OCR.
//
// A document the inspector reports with no per-page detail (every native
// format) is extracted whole: nil pages means "all of it".
func partition(insp *engine.Inspection) (native []int, ocr []int) {
	if len(insp.Pages) == 0 {
		return nil, nil
	}
	for _, p := range insp.Pages {
		switch p.Classification.Type {
		case canonical.PageTypeScanned, canonical.PageTypeImageBased:
			ocr = append(ocr, p.Number)
		default:
			// Text-based and mixed pages are extracted natively first. A mixed
			// page usually has recoverable text, and escalation catches it
			// when it does not -- trying the cheap engine first is the whole
			// point.
			native = append(native, p.Number)
		}
	}
	return native, ocr
}

func placeholderPage(number int, insp *engine.Inspection, reason string) canonical.Page {
	page := canonical.Page{
		Number: number,
		Kind:   insp.PageKind,
		Classification: canonical.Classification{
			Type:    canonical.PageTypeScanned,
			Reasons: []string{reason},
		},
		Blocks: []canonical.Block{},
	}
	for _, p := range insp.Pages {
		if p.Number == number {
			page.Classification = p.Classification
			page.Classification.Reasons = append(append([]string(nil), p.Classification.Reasons...), reason)
			break
		}
	}
	return page
}

func union(a, b []int) []int {
	seen := make(map[int]bool, len(a)+len(b))
	var out []int
	for _, xs := range [][]int{a, b} {
		for _, x := range xs {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	sort.Ints(out)
	return out
}

func intSet(xs []int) map[int]bool {
	m := make(map[int]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// mergeMetadata fills gaps in dst from src without overwriting what inspection
// already established.
func mergeMetadata(dst *canonical.Metadata, src canonical.Metadata) {
	if dst.Title == "" {
		dst.Title = src.Title
	}
	if dst.Author == "" {
		dst.Author = src.Author
	}
	if dst.Language == "" {
		dst.Language = src.Language
	}
	if len(src.Custom) > 0 {
		if dst.Custom == nil {
			dst.Custom = make(map[string]string, len(src.Custom))
		}
		for k, v := range src.Custom {
			if _, exists := dst.Custom[k]; !exists {
				dst.Custom[k] = v
			}
		}
	}
}

// IsUnsupported reports whether an error means no engine could handle the
// input, which the API surfaces as a 415 rather than a 500.
func IsUnsupported(err error) bool { return errors.Is(err, engine.ErrUnsupported) }

// IsBadDocument reports whether an error means the document itself is broken,
// which the API surfaces as a 422.
func IsBadDocument(err error) bool {
	return errors.Is(err, engine.ErrMalformed) || errors.Is(err, engine.ErrEncrypted)
}
