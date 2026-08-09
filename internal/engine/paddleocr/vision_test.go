package paddleocr_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/engine/paddleocr"
)

// visionResponse is what the service returns for tier=vision: the same envelope
// as OCR, answering under a different engine name.
func visionResponse(page int) map[string]any {
	return map[string]any{
		"schema_version": canonical.SchemaVersion,
		"engine":         "mineru",
		"engine_version": "2.5.4",
		"metadata":       map[string]any{"page_count": 1},
		"duration_ms":    9000,
		"pages": []any{map[string]any{
			"number": page,
			"kind":   "paginated",
			"width":  612.0,
			"height": 792.0,
			"classification": map[string]any{
				"type": "scanned", "confidence": 0.9, "reasons": []string{"vision"},
			},
			"blocks": []any{map[string]any{
				"id": "p1-v0", "type": "paragraph",
				"text": "Read by the vision tier.",
				"provenance": map[string]any{
					"engine": "mineru", "engine_version": "2.5.4",
					"method": "mineru/hybrid-engine:text",
				},
			}},
		}},
	}
}

func visionEngine(t *testing.T, svc *fakeService) (*paddleocr.Engine, *paddleocr.VisionEngine) {
	t.Helper()
	ocr, err := paddleocr.New(svc.start(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ocr, paddleocr.NewVision(ocr)
}

// A service without MinerU installed must not produce a Tier 3 the router
// would then call and always fail.
func TestNewVisionIsNilWhenTheServiceHasNoVisionTier(t *testing.T) {
	_, v := visionEngine(t, &fakeService{visionAvailable: false})
	if v != nil {
		t.Fatalf("expected no vision engine, got %+v", v)
	}
}

func TestNewVisionIsNilWithoutAnOCRClient(t *testing.T) {
	if v := paddleocr.NewVision(nil); v != nil {
		t.Fatalf("expected nil, got %+v", v)
	}
}

func TestVisionEngineIdentity(t *testing.T) {
	_, v := visionEngine(t, &fakeService{visionAvailable: true, engineVersion: "3.7.0"})
	if v == nil {
		t.Fatal("expected a vision engine")
	}
	if v.Name() != paddleocr.VisionName {
		t.Errorf("Name = %q, want %q", v.Name(), paddleocr.VisionName)
	}
	// The OCR tier's version is not MinerU's, and it is part of the cache key.
	if got := v.Version(); got != "unknown" {
		t.Errorf("Version = %q before any call; want unknown", got)
	}
}

// Tier 3 answers as a different engine on purpose, so the extract path must
// not apply the OCR tier's engine-identity check.
func TestVisionExtractAsksForTheVisionTier(t *testing.T) {
	svc := &fakeService{visionAvailable: true, visionBody: visionResponse(4)}
	_, v := visionEngine(t, svc)

	res, err := v.Extract(context.Background(), request(t, 4))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if svc.gotTier != "vision" {
		t.Errorf("tier field = %q, want vision", svc.gotTier)
	}
	if svc.gotPages != "4" {
		t.Errorf("pages field = %q, want 4", svc.gotPages)
	}
	if len(res.Pages) != 1 || res.Pages[0].Number != 4 {
		t.Fatalf("pages = %+v", res.Pages)
	}
	if got := res.Pages[0].Blocks[0].Provenance.Engine; got != "mineru" {
		t.Errorf("provenance engine = %q, want mineru", got)
	}
}

// The version the service reports for the vision model is not the OCR version,
// and Tier 3 should adopt it once it has seen a real answer.
func TestVisionAdoptsTheReportedEngineVersion(t *testing.T) {
	svc := &fakeService{visionAvailable: true, engineVersion: "3.7.0", visionBody: visionResponse(1)}
	_, v := visionEngine(t, svc)

	if _, err := v.Extract(context.Background(), request(t, 1)); err != nil {
		t.Fatal(err)
	}
	if v.Version() != "2.5.4" {
		t.Errorf("Version = %q, want the reported 2.5.4", v.Version())
	}
}

// Vision is an escalation for named pages. A whole-document request would be
// an accident -- most likely a caller treating it as an ordinary engine.
func TestVisionRefusesAWholeDocument(t *testing.T) {
	_, v := visionEngine(t, &fakeService{visionAvailable: true})

	_, err := v.Extract(context.Background(), request(t))
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestVisionNeverInspectsOrCompetes(t *testing.T) {
	_, v := visionEngine(t, &fakeService{visionAvailable: true})

	if _, err := v.Inspect(context.Background(), canonical.Source{}, "doc.pdf"); !errors.Is(err, engine.ErrUnsupported) {
		t.Errorf("Inspect err = %v, want ErrUnsupported", err)
	}
	if s := v.Supports(&engine.Inspection{}); s != engine.SupportNone {
		t.Errorf("Supports = %v, want SupportNone", s)
	}
}

// Tier 3 does not shard: the service reads a page at a time regardless, so
// splitting would only upload the document more than once.
func TestVisionSendsOneRequestForAllPages(t *testing.T) {
	svc := &shardingService{workers: 4, vision: true}
	ocr, err := paddleocr.New(svc.start(t))
	if err != nil {
		t.Fatal(err)
	}
	v := paddleocr.NewVision(ocr)
	if v == nil {
		t.Fatal("expected a vision engine")
	}

	if _, err := v.Extract(context.Background(), request(t, 2, 5, 9)); err != nil {
		t.Fatal(err)
	}
	if got := svc.sentPages(); len(got) != 1 || got[0] != "2,5,9" {
		t.Errorf("requests = %v, want one request for 2,5,9", got)
	}
}

// The tiers share the service's worker pool, so they must share one budget.
// Separate semaphores would let 2 OCR shards and a vision request oversubscribe
// a 2-worker service.
func TestVisionSharesTheOCRConcurrencyBudget(t *testing.T) {
	svc := &shardingService{workers: 2, delay: 120 * time.Millisecond, vision: true}
	ocr, err := paddleocr.New(svc.start(t))
	if err != nil {
		t.Fatal(err)
	}
	v := paddleocr.NewVision(ocr)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := ocr.Extract(context.Background(), request(t, 1, 2, 3, 4)); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := v.Extract(context.Background(), request(t, 5, 6)); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	switch peak := svc.peakConcurrency(); {
	case peak > 2:
		t.Errorf("peak concurrency = %d; the tiers are not sharing a budget", peak)
	case peak < 2:
		t.Errorf("peak concurrency = %d; the requests never overlapped, so this "+
			"run proves nothing about the budget", peak)
	}
}

// A cancelled context must be honoured while queued for a slot, not only in
// flight -- otherwise a shutdown waits for every escalation ahead of it.
func TestVisionHonoursCancellationWhileWaiting(t *testing.T) {
	svc := &shardingService{workers: 1, delay: time.Second, vision: true}
	ocr, err := paddleocr.New(svc.start(t))
	if err != nil {
		t.Fatal(err)
	}
	v := paddleocr.NewVision(ocr)

	busy := make(chan struct{})
	go func() {
		defer close(busy)
		_, _ = ocr.Extract(context.Background(), request(t, 1))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err = v.Extract(ctx, request(t, 2))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Errorf("waited %s; cancellation was not honoured until the slot freed", elapsed)
	}
	<-busy
}

// Runs only against a service started with `make ocr-vision`. Self-hosting the
// vision model is what makes this testable at all -- there are no credentials
// to lack, so the live path is exercised here rather than assumed.
func TestVisionAgainstTheRealService(t *testing.T) {
	url := os.Getenv("DOLICO_OCR_URL")
	if url == "" {
		t.Skip("set DOLICO_OCR_URL to test against a running OCR service")
	}
	ocr, err := paddleocr.New(url, paddleocr.WithTimeout(10*time.Minute))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v := paddleocr.NewVision(ocr)
	if v == nil {
		t.Skip("the service has no vision tier; start it with `make ocr-vision`")
	}

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.Extract(context.Background(), &engine.ExtractRequest{
		Source: canonical.Source{Filename: "scanned-table.pdf"},
		Path:   filepath.Join(root, "testdata", "scanned-table.pdf"),
		Pages:  []int{1},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Pages) != 1 || res.Pages[0].Number != 1 {
		t.Fatalf("pages = %+v", res.Pages)
	}

	var table *canonical.Table
	for _, b := range res.Pages[0].Blocks {
		if b.Provenance.Engine != paddleocr.VisionName {
			t.Errorf("block %s came back as engine %q", b.ID, b.Provenance.Engine)
		}
		// MinerU reports no confidence and this pipeline does not invent one.
		if b.Confidence != nil {
			t.Errorf("block %s carries a confidence the model never reported", b.ID)
		}
		if b.Type == canonical.BlockTable {
			table = b.Table
		}
	}
	if table == nil {
		t.Fatal("the vision tier did not find the table on this page")
	}
	if len(table.Grid) != 5 || len(table.Grid[0]) != 3 {
		t.Fatalf("grid is %d rows; want the fixture's 5x3", len(table.Grid))
	}
	// Reading order and the coordinate flip both have to be right for this to
	// land in row 0: get either wrong and the grid arrives upside down.
	if got := table.Grid[0][0].Blocks[0].Text; got != "Region" {
		t.Errorf("first cell = %q, want Region", got)
	}
	if got := table.Grid[4][2].Blocks[0].Text; got != "6,480.00" {
		t.Errorf("last cell = %q, want 6,480.00", got)
	}
}

// The service's own failures must reach the router as errors, not as an empty
// page it would then treat as a bad read.
func TestVisionSurfacesServiceFailures(t *testing.T) {
	svc := &fakeService{
		visionAvailable: true,
		extractStatus:   503,
		extractBody:     map[string]string{"kind": "unavailable", "message": "mineru not installed"},
	}
	_, v := visionEngine(t, svc)

	_, err := v.Extract(context.Background(), request(t, 1))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "mineru not installed") {
		t.Errorf("err = %v; the service's reason should survive", err)
	}
}
