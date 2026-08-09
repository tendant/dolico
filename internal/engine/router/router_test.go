package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/tendant/dolico/internal/cache"
	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
)

// fakeEngine lets a test state exactly what an engine reports and records what
// the router asked it for. The routing policy is the thing under test, so the
// engines have to be fully controllable.
type fakeEngine struct {
	name       string
	version    string
	inspection *engine.Inspection
	inspectErr error
	support    engine.SupportScore
	extractErr error

	// text returned for each extracted page; missing pages get a default.
	text map[int]string

	// confidence, when set for a page, makes its block report a measured
	// confidence — what an OCR tier does and a native parser does not.
	confidence map[int]float64

	// emptyPages makes the engine return pages with no blocks — what a vision
	// tier does when it reads a page and finds nothing.
	emptyPages bool

	assets []canonical.Asset

	// calls records the page sets passed to Extract, in order.
	calls [][]int
}

func (f *fakeEngine) Name() string    { return f.name }
func (f *fakeEngine) Version() string { return f.version }

func (f *fakeEngine) Inspect(context.Context, canonical.Source, string) (*engine.Inspection, error) {
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	return f.inspection, nil
}

func (f *fakeEngine) Supports(*engine.Inspection) engine.SupportScore { return f.support }

func (f *fakeEngine) Extract(_ context.Context, req *engine.ExtractRequest) (*engine.ExtractResult, error) {
	f.calls = append(f.calls, append([]int(nil), req.Pages...))
	if f.extractErr != nil {
		return nil, f.extractErr
	}
	pages := req.Pages
	if len(pages) == 0 {
		pages = []int{1}
	}
	out := make([]canonical.Page, 0, len(pages))
	for _, n := range pages {
		text, ok := f.text[n]
		if !ok {
			text = "Extracted content for this page is long enough to score as plausible prose."
		}
		page := canonical.Page{
			Number:         n,
			Kind:           canonical.PageKindPaginated,
			Classification: canonical.Classification{Type: canonical.PageTypeTextBased, Confidence: 1},
		}
		if !f.emptyPages {
			block := canonical.Block{
				ID:         fmt.Sprintf("p%d-b0", n),
				Type:       canonical.BlockParagraph,
				Text:       text,
				Provenance: canonical.Provenance{Engine: f.name, EngineVersion: f.version, Method: "fake"},
			}
			// An engine that measures reports how sure it was; one that reads a
			// data structure reports nothing. The scorer treats them
			// differently, so the fake has to be able to be either.
			if conf, ok := f.confidence[n]; ok {
				c := conf
				block.Confidence = &c
			}
			page.Blocks = []canonical.Block{block}
		}
		out = append(out, page)
	}
	return &engine.ExtractResult{Pages: out, Assets: f.assets}, nil
}

func inspection(engineName string, kinds ...canonical.PageType) *engine.Inspection {
	insp := &engine.Inspection{
		PageCount: len(kinds),
		PageKind:  canonical.PageKindPaginated,
		Engine:    engineName,
	}
	for i, k := range kinds {
		insp.Pages = append(insp.Pages, canonical.Page{
			Number:         i + 1,
			Kind:           canonical.PageKindPaginated,
			Classification: canonical.Classification{Type: k, Confidence: 1},
		})
	}
	return insp
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newRouter(t *testing.T, primary, ocr engine.Engine) *Router {
	t.Helper()
	engines := []engine.Engine{primary}
	if ocr != nil {
		engines = append(engines, ocr)
	}
	return New(engine.NewRegistry(engines...), ocr, cache.New(0), Options{
		OCRThreshold: 0.6,
		Logger:       testLogger(),
	})
}

func request() Request {
	return Request{
		DocumentID: "doc1",
		TraceID:    "trace1",
		Source:     canonical.Source{Filename: "x.pdf", SHA256: "abc", MediaType: "application/pdf"},
		Path:       "/dev/null",
	}
}

// The central rule of the design: a PDF with one text page and one scan costs
// one OCR call, not two and not zero.
func TestMixedDocumentRoutesEachPageToItsOwnEngine(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased, canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", support: engine.SupportNone}
	ocr.inspectErr = engine.ErrUnsupported

	doc, err := newRouter(t, primary, ocr).Process(context.Background(), request())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	if len(primary.calls) != 1 || len(primary.calls[0]) != 1 || primary.calls[0][0] != 1 {
		t.Errorf("primary should have extracted only page 1, got %v", primary.calls)
	}
	if len(ocr.calls) != 1 || len(ocr.calls[0]) != 1 || ocr.calls[0][0] != 2 {
		t.Errorf("ocr should have extracted only page 2, got %v", ocr.calls)
	}
	if len(doc.Pages) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(doc.Pages))
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != "pdf" {
		t.Errorf("page 1 engine = %s, want pdf", got)
	}
	if got := doc.Pages[1].Blocks[0].Provenance.Engine; got != "ocr" {
		t.Errorf("page 2 engine = %s, want ocr", got)
	}
}

func TestAllTextDocumentNeverCallsOCR(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased, canonical.PageTypeTextBased),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported}

	if _, err := newRouter(t, primary, ocr).Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(ocr.calls) != 0 {
		t.Errorf("OCR was called for an all-text document: %v", ocr.calls)
	}
}

func TestImageBasedPagesGoStraightToOCR(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeImageBased),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported}

	if _, err := newRouter(t, primary, ocr).Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(primary.calls) != 0 {
		t.Errorf("primary should not extract an image-only page, got %v", primary.calls)
	}
	if len(ocr.calls) != 1 {
		t.Errorf("ocr should have been called once, got %v", ocr.calls)
	}
}

// A mixed page is tried cheaply first; that is the entire point of preferring
// native extraction.
func TestMixedPageIsTriedNativelyBeforeOCR(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeMixed),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported}

	if _, err := newRouter(t, primary, ocr).Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(primary.calls) != 1 {
		t.Errorf("mixed page should be extracted natively first, got %v", primary.calls)
	}
}

// A page can extract "successfully" and be garbage. Quality scoring, not the
// engine's own confidence, is what catches it.
func TestLowQualityPageEscalatesToOCR(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased, canonical.PageTypeTextBased),
		text: map[int]string{
			1: "This page extracted cleanly and reads like ordinary English prose about a subject.",
			2: "��� �� ����",
		},
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported}

	doc, err := newRouter(t, primary, ocr).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(ocr.calls) != 1 || ocr.calls[0][0] != 2 {
		t.Fatalf("page 2 should have escalated to OCR, got %v", ocr.calls)
	}
	if q := doc.Pages[1].Quality; q == nil || !q.Escalated {
		t.Errorf("page 2 quality should be marked escalated, got %+v", q)
	}
	if q := doc.Pages[0].Quality; q == nil || q.Escalated {
		t.Errorf("page 1 should not be marked escalated, got %+v", q)
	}
}

// A failing OCR tier must not throw away the pages that extracted fine.
func TestOCRFailureKeepsTheGoodPages(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased, canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{
		name: "ocr", version: "1",
		inspectErr: engine.ErrUnsupported,
		extractErr: errors.New("ocr service is down"),
	}

	doc, err := newRouter(t, primary, ocr).Process(context.Background(), request())
	if err != nil {
		t.Fatalf("a failed OCR tier should not fail the document: %v", err)
	}
	if len(doc.Pages) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(doc.Pages))
	}
	if len(doc.Pages[0].Blocks) == 0 {
		t.Error("the natively-extracted page was discarded")
	}
	if !hasReason(doc.Pages[1], "ocr_failed") {
		t.Errorf("page 2 should record why it is empty, got %v", doc.Pages[1].Classification.Reasons)
	}
	// The failure has to be visible in the trace, not only in the logs.
	var found bool
	for _, run := range doc.Trace.Engines {
		if run.Engine == "ocr" && run.Error != "" {
			found = true
		}
	}
	if !found {
		t.Error("the OCR failure should appear in the document trace")
	}
}

func TestNoOCREngineStillProducesADocument(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	doc, err := newRouter(t, primary, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(doc.Pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(doc.Pages))
	}
	if !hasReason(doc.Pages[0], "no_ocr_engine") {
		t.Errorf("reasons = %v, want no_ocr_engine", doc.Pages[0].Classification.Reasons)
	}
}

func TestNativeDocumentExtractsAsAWhole(t *testing.T) {
	// A native format reports no per-page detail, so the router must extract
	// the whole document rather than nothing.
	primary := &fakeEngine{
		name:       "anydoc",
		version:    "1",
		support:    engine.SupportNative,
		inspection: &engine.Inspection{PageCount: 1, PageKind: canonical.PageKindSection, Engine: "anydoc"},
	}
	doc, err := newRouter(t, primary, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(primary.calls) != 1 || len(primary.calls[0]) != 0 {
		t.Errorf("expected one whole-document extract (nil pages), got %v", primary.calls)
	}
	if len(doc.Pages) != 1 || len(doc.Pages[0].Blocks) == 0 {
		t.Errorf("expected one page with blocks, got %+v", doc.Pages)
	}
}

func TestPagesAreAssembledInOrderWithGapsFilled(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf",
			canonical.PageTypeTextBased, canonical.PageTypeTextBased, canonical.PageTypeTextBased),
	}
	// The engine returns only pages 1 and 3, dropping 2.
	primary.text = map[int]string{}
	r := newRouter(t, primary, nil)
	doc, err := r.Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Pages) != 3 {
		t.Fatalf("expected 3 pages, got %d", len(doc.Pages))
	}
	for i, p := range doc.Pages {
		if p.Number != i+1 {
			t.Errorf("page %d out of order: got number %d", i, p.Number)
		}
	}
}

func TestSecondRunHitsThePageCache(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased),
	}
	r := newRouter(t, primary, nil)
	if _, err := r.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(primary.calls) != 1 {
		t.Errorf("the second run should have been served from cache, got %d extract calls", len(primary.calls))
	}
}

// The page cache exists so an engine upgrade re-runs only that engine's pages.
func TestEngineVersionBumpInvalidatesThatEnginesPages(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased),
	}
	shared := cache.New(0)
	opts := Options{OCRThreshold: 0.6, Logger: testLogger()}

	r1 := New(engine.NewRegistry(primary), nil, shared, opts)
	if _, err := r1.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	primary.version = "2"
	r2 := New(engine.NewRegistry(primary), nil, shared, opts)
	if _, err := r2.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(primary.calls) != 2 {
		t.Errorf("a version bump should force re-extraction, got %d calls", len(primary.calls))
	}
}

func TestAssetsSurviveACacheHit(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased),
		assets: []canonical.Asset{{
			ID: "a0", MediaType: "image/png", BlobRef: "img.png", SizeBytes: 10,
		}},
	}
	r := newRouter(t, primary, nil)
	if _, err := r.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	doc, err := r.Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Assets) != 1 || doc.Assets[0].ID != "a0" {
		t.Errorf("a cache hit dropped the document's assets: %+v", doc.Assets)
	}
}

func TestUnsupportedInputIsReportedAsSuch(t *testing.T) {
	primary := &fakeEngine{name: "pdf", version: "1", inspectErr: engine.ErrUnsupported}
	_, err := newRouter(t, primary, nil).Process(context.Background(), request())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsUnsupported(err) {
		t.Errorf("IsUnsupported(%v) = false, want true", err)
	}
}

func TestMalformedInputIsReportedAsSuch(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1",
		inspectErr: fmt.Errorf("%w: broken xref", engine.ErrMalformed),
	}
	_, err := newRouter(t, primary, nil).Process(context.Background(), request())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsBadDocument(err) {
		t.Errorf("IsBadDocument(%v) = false, want true", err)
	}
}

func TestTraceRecordsEveryEngineRun(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased, canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "9", inspectErr: engine.ErrUnsupported}

	doc, err := newRouter(t, primary, ocr).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Trace.TraceID != "trace1" {
		t.Errorf("trace id = %q", doc.Trace.TraceID)
	}
	if doc.Trace.PipelineVersion != canonical.PipelineVersion {
		t.Errorf("pipeline version = %q", doc.Trace.PipelineVersion)
	}
	seen := map[string]bool{}
	for _, run := range doc.Trace.Engines {
		seen[run.Engine] = true
	}
	for _, want := range []string{"pdf:inspect", "pdf", "ocr"} {
		if !seen[want] {
			t.Errorf("trace is missing a run for %q: %+v", want, doc.Trace.Engines)
		}
	}
}

// ---------------------------------------------------------------------------
// Tier 3 escalation
// ---------------------------------------------------------------------------

// garbage scores below any sane threshold; clean scores well above it.
const (
	garbage = "��� �� ����"
	clean   = "This page reads as ordinary English prose about a subject at hand."
)

func visionRouter(t *testing.T, primary, ocr, vision engine.Engine, opts func(*Options)) *Router {
	t.Helper()
	o := Options{
		OCRThreshold:    0.6,
		VisionThreshold: 0.35,
		VisionMaxPages:  5,
		Logger:          testLogger(),
	}
	if opts != nil {
		opts(&o)
	}
	engines := []engine.Engine{primary}
	if ocr != nil {
		engines = append(engines, ocr)
	}
	return New(engine.NewRegistry(engines...), ocr, cache.New(0), o).WithVision(vision)
}

// The core claim: a page OCR read badly gets a second escalation.
func TestBadOCRPageEscalatesToVision(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: garbage}}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: clean}}

	doc, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(vision.calls) != 1 || vision.calls[0][0] != 1 {
		t.Fatalf("vision should have read page 1, got %v", vision.calls)
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != "mineru" {
		t.Errorf("page engine = %s, want mineru", got)
	}
	if q := doc.Pages[0].Quality; q == nil || !q.Escalated {
		t.Errorf("the page should be marked escalated, got %+v", q)
	}
	if !hasReason(doc.Pages[0], "vision_escalated") {
		t.Errorf("reasons = %v", doc.Pages[0].Classification.Reasons)
	}
}

// A page OCR read well must not pay for Tier 3.
func TestGoodOCRPageDoesNotEscalate(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: clean}}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported}

	if _, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(vision.calls) != 0 {
		t.Errorf("vision ran on a page OCR read fine: %v", vision.calls)
	}
}

// A natively-extracted page never reaches Tier 3, however it scored — it was
// never handed to OCR, so it is not an OCR failure to recover.
func TestNativePageNeverReachesVision(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased),
		text:       map[int]string{1: clean},
	}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported}

	if _, err := visionRouter(t, primary, nil, vision, nil).Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(vision.calls) != 0 {
		t.Errorf("vision ran on a natively-extracted page: %v", vision.calls)
	}
}

// Tier 3 must never make a page worse.
func TestVisionFailureKeepsTheOCRResult(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: garbage}}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported,
		extractErr: errors.New("mineru fell over")}

	doc, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatalf("a failed vision tier must not fail the document: %v", err)
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != "ocr" {
		t.Errorf("the OCR result should survive, got engine %s", got)
	}
	if !hasReason(doc.Pages[0], "vision_failed") {
		t.Errorf("the failure should be visible in reasons, got %v", doc.Pages[0].Classification.Reasons)
	}
}

// An empty vision result is worse than the bad text we already had.
func TestEmptyVisionResultKeepsTheOCRResult(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: garbage}}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported,
		emptyPages: true}

	doc, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Pages[0].Blocks) == 0 {
		t.Fatal("the OCR blocks were discarded for an empty vision result")
	}
	if !hasReason(doc.Pages[0], "vision_empty") {
		t.Errorf("reasons = %v", doc.Pages[0].Classification.Reasons)
	}
}

// The cap must keep the pages that need it most, and say what it dropped.
func TestVisionCapEscalatesTheWorstPages(t *testing.T) {
	kinds := make([]canonical.PageType, 4)
	for i := range kinds {
		kinds[i] = canonical.PageTypeScanned
	}
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", kinds...),
	}
	// Page 3 is the worst (nothing at all), then 1 and 2; page 4 is clean.
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{
			1: garbage,
			2: garbage + garbage,
			3: "",
			4: clean,
		}}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: clean, 2: clean, 3: clean}}

	r := visionRouter(t, primary, ocr, vision, func(o *Options) { o.VisionMaxPages = 2 })
	if _, err := r.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(vision.calls) != 1 {
		t.Fatalf("expected one vision call, got %v", vision.calls)
	}
	got := vision.calls[0]
	if len(got) != 2 {
		t.Fatalf("the cap should have limited this to 2 pages, got %v", got)
	}
	// Page 4 read fine and must not be among them.
	if slices.Contains(got, 4) {
		t.Errorf("a well-read page was escalated: %v", got)
	}
}

// A page's quality must describe the text actually on it. Carrying the
// pre-OCR score forward looks harmless until Tier 3 reads it: a page OCR
// rescued would still be escalated, and a page OCR ruined would not be.
func TestAnOCRPageIsScoredOnWhatOCRProduced(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeTextBased),
		text:       map[int]string{1: garbage}, // scores below the OCR threshold
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: clean}} // ...and OCR rescues it
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported}

	doc, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	q := doc.Pages[0].Quality
	if q == nil {
		t.Fatal("the page has no quality")
	}
	if q.Score < 0.6 {
		t.Errorf("score = %v; that is the pre-OCR score, not the OCR result's", q.Score)
	}
	if !q.Escalated {
		t.Error("the page was escalated to OCR and should say so")
	}
	if len(vision.calls) != 0 {
		t.Errorf("vision ran on a page OCR had already rescued: %v", vision.calls)
	}
}

// The case the whole scorer change exists for, end to end through the router:
// OCR returns text that looks completely fine and is wrong, and says so only
// in its confidence.
func TestAConfidentMisreadEscalatesToVision(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{
		name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		// Word-shaped, damage-free, and misread.
		text:       map[int]string{1: "Rcglon Unlts Rcvcnuc Nortb l2O l4,4OO.OO"},
		confidence: map[int]float64{1: 0.38},
	}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: clean}}

	doc, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(vision.calls) != 1 {
		t.Fatalf("a page OCR was 38%% sure of did not escalate: %v", vision.calls)
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != "mineru" {
		t.Errorf("page engine = %s, want mineru", got)
	}
}

// ...and the converse, which is what keeps Tier 3 a fallback: the same
// well-formed text, read confidently, stays put.
func TestAConfidentGoodReadDoesNotEscalate(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{
		name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text:       map[int]string{1: clean},
		confidence: map[int]float64{1: 0.96},
	}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported}

	if _, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(vision.calls) != 0 {
		t.Errorf("vision ran on a page read at 96%% confidence: %v", vision.calls)
	}
}

// The measurement has to reach the page's quality record, not just the score,
// so an operator can see why a page was or was not escalated.
func TestTheMeasurementIsRecordedOnThePage(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{
		name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text:       map[int]string{1: clean},
		confidence: map[int]float64{1: 0.91},
	}

	doc, err := visionRouter(t, primary, ocr, nil, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	q := doc.Pages[0].Quality
	if q == nil || q.MeasuredConfidence == nil {
		t.Fatalf("no measurement on an OCR page: %+v", q)
	}
	if *q.MeasuredConfidence != 0.91 {
		t.Errorf("measured confidence = %v, want 0.91", *q.MeasuredConfidence)
	}
	if q.MeasuredCoverage != 1 {
		t.Errorf("coverage = %v; every character came from a scored block", q.MeasuredCoverage)
	}
}

func TestNoVisionEngineIsATwoTierRun(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: garbage}}

	doc, err := visionRouter(t, primary, ocr, nil, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != "ocr" {
		t.Errorf("engine = %s, want the OCR result to stand", got)
	}
}

// A zero threshold disables the tier without needing the engine removed.
func TestZeroThresholdDisablesVision(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: garbage}}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported}

	r := visionRouter(t, primary, ocr, vision, func(o *Options) { o.VisionThreshold = 0 })
	if _, err := r.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(vision.calls) != 0 {
		t.Errorf("vision ran with the threshold at zero: %v", vision.calls)
	}
}

func TestVisionRunAppearsInTheTrace(t *testing.T) {
	primary := &fakeEngine{
		name: "pdf", version: "1", support: engine.SupportNative,
		inspection: inspection("pdf", canonical.PageTypeScanned),
	}
	ocr := &fakeEngine{name: "ocr", version: "1", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: garbage}}
	vision := &fakeEngine{name: "mineru", version: "2.5", inspectErr: engine.ErrUnsupported,
		text: map[int]string{1: clean}}

	doc, err := visionRouter(t, primary, ocr, vision, nil).Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, run := range doc.Trace.Engines {
		if run.Engine == "mineru" && slices.Equal(run.Pages, []int{1}) {
			found = true
		}
	}
	if !found {
		t.Errorf("the vision run is missing from the trace: %+v", doc.Trace.Engines)
	}
}

func hasReason(p canonical.Page, want string) bool {
	return slices.Contains(p.Classification.Reasons, want)
}
