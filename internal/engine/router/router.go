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
	"slices"
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
	// VisionProbe asks the vision tier about one page of every document that
	// used OCR, even when no page admitted to failing.
	//
	// It exists because the threshold above can only catch OCR that knows it
	// struggled. Measured on the repository's own corpus, the OCR tier misread
	// more than half the words on a real microfilm scan and reported 0.938
	// confidence — a page that scores 0.61 and is never escalated. Nothing
	// computed from that page disagrees with the engine. Another engine does.
	VisionProbe bool
	// VisionDisagreement is how far the two tiers must be apart on the probed
	// page before the OCR tier is distrusted for the whole document.
	VisionDisagreement float64
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
	if opts.VisionDisagreement <= 0 {
		opts.VisionDisagreement = quality.DefaultDisagreement
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
	if r.vision == nil || len(ocrPages) == 0 {
		return
	}
	if r.opts.VisionThreshold <= 0 && !r.opts.VisionProbe {
		return
	}

	// Rank every OCR page, worst first. Both the threshold and the probe want
	// the same ordering: a cap should keep the pages that need it most, and
	// the page most likely to expose a failing engine is the one it already
	// scored lowest.
	ranked := r.rankOCRPages(ocrPages, byNumber)
	if len(ranked) == 0 {
		return
	}

	var below []int
	for _, c := range ranked {
		if c.score < r.opts.VisionThreshold {
			below = append(below, c.number)
		}
	}

	targets := below
	probed, probeResult := -1, (*engine.ExtractResult)(nil)
	if r.opts.VisionProbe {
		probed, probeResult = r.probeVision(ctx, req, ranked[0].number, byNumber, doc, log)
		if probeResult != nil {
			// The probe disagreed: this document's OCR is not to be trusted
			// anywhere, not just on the pages that admitted trouble.
			targets = make([]int, 0, len(ranked))
			for _, c := range ranked {
				targets = append(targets, c.number)
			}
		}
	}
	if len(targets) == 0 {
		return
	}

	// Re-rank the targets worst-first before capping, since `targets` may now
	// be every OCR page rather than only the ones under the threshold.
	pages, dropped := r.capWorstFirst(targets, ranked)

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

	// The probe already read one page. Asking for it again would pay for the
	// same inference twice.
	remaining := make([]int, 0, len(pages))
	for _, n := range pages {
		if n != probed || probeResult == nil {
			remaining = append(remaining, n)
		}
	}

	res := &engine.ExtractResult{}
	if probeResult != nil {
		for _, p := range probeResult.Pages {
			if slices.Contains(pages, p.Number) {
				res.Pages = append(res.Pages, p)
			}
		}
	}
	if len(remaining) > 0 {
		more, err := r.extract(ctx, r.vision, req, remaining, doc)
		if err != nil {
			log.Error("vision extraction failed; keeping the OCR results",
				"pages", remaining, "error", err)
			for _, number := range remaining {
				markPage(byNumber, number, "vision_failed")
			}
		} else {
			res.Pages = append(res.Pages, more.Pages...)
			res.Assets = append(res.Assets, more.Assets...)
		}
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
		if page.Number == probed {
			// Recorded here rather than on the OCR page, which this replaces:
			// marking it before the swap would put the reason on a page that
			// no longer exists. It says which page the decision was made from.
			page.Classification.Reasons = append(page.Classification.Reasons, "vision_probe")
		}
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

// scored is one OCR page and what its OCR result scored.
type scored struct {
	number int
	score  float64
}

// rankOCRPages scores every page the OCR tier produced and returns them worst
// first.
//
// Pages that went straight to OCR have not been scored at this point — step 4
// only scores natively-extracted ones — so they are scored here. Skipping them
// would make the commonest candidate, a scanned page, unreachable.
func (r *Router) rankOCRPages(ocrPages []int, byNumber map[int]canonical.Page) []scored {
	ranked := make([]scored, 0, len(ocrPages))
	for _, number := range ocrPages {
		page, ok := byNumber[number]
		if !ok {
			continue
		}
		if page.Quality == nil {
			q := quality.Score(&page, r.opts.Weights)
			page.Quality = &q
			byNumber[number] = page
		}
		ranked = append(ranked, scored{number, page.Quality.Score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score < ranked[j].score
		}
		return ranked[i].number < ranked[j].number
	})
	return ranked
}

// capWorstFirst trims a target set to VisionMaxPages, keeping the worst
// scorers, and returns the kept pages in page order along with how many were
// dropped.
func (r *Router) capWorstFirst(targets []int, ranked []scored) (pages []int, dropped int) {
	want := make(map[int]bool, len(targets))
	for _, n := range targets {
		want[n] = true
	}
	for _, c := range ranked {
		if !want[c.number] {
			continue
		}
		if len(pages) >= r.opts.VisionMaxPages {
			dropped++
			continue
		}
		pages = append(pages, c.number)
	}
	sort.Ints(pages)
	return pages, dropped
}

// probeVision reads one page with the vision tier and reports whether the two
// tiers are telling the same story about this document.
//
// It returns the probe's result only when they disagree. That is deliberate: a
// probe that agrees is discarded rather than applied, so a document whose OCR
// was fine keeps one engine throughout instead of having a single arbitrary
// page swapped for another engine's rendering of the same words.
//
// A probe failure is not a page failure. The page was never a target — it was
// a question — so nothing is marked and the caller carries on with whatever
// the threshold selected.
func (r *Router) probeVision(
	ctx context.Context,
	req Request,
	number int,
	byNumber map[int]canonical.Page,
	doc *canonical.Document,
	log *slog.Logger,
) (int, *engine.ExtractResult) {
	before, ok := byNumber[number]
	if !ok {
		return -1, nil
	}

	res, err := r.extract(ctx, r.vision, req, []int{number}, doc)
	if err != nil {
		log.Warn("vision probe failed; falling back to the quality threshold alone",
			"page", number, "error", err)
		return -1, nil
	}
	var after *canonical.Page
	for i := range res.Pages {
		if res.Pages[i].Number == number {
			after = &res.Pages[i]
		}
	}
	if after == nil || len(after.Blocks) == 0 {
		log.Warn("vision probe returned nothing; falling back to the quality threshold alone",
			"page", number)
		return -1, nil
	}

	d := quality.Disagreement(quality.PlainText(&before), quality.PlainText(after))
	if d < r.opts.VisionDisagreement {
		log.Info("vision probe agrees with OCR; keeping the OCR results",
			"page", number, "disagreement", d, "threshold", r.opts.VisionDisagreement)
		return number, nil
	}
	log.Info("vision probe disagrees with OCR; distrusting it for this document",
		"page", number, "disagreement", d, "threshold", r.opts.VisionDisagreement)
	return number, res
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
